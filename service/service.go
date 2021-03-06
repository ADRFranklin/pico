// Package service provides the runnable service that acts as the root of the
// overall system. It provides a configuration structure, a way to initialise a
// primed instance of the service which can then be start via the .Start() func.
package service

import (
	"context"
	"time"

	"github.com/eapache/go-resiliency/retrier"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"gopkg.in/src-d/go-git.v4/plumbing/transport"
	"gopkg.in/src-d/go-git.v4/plumbing/transport/http"
	"gopkg.in/src-d/go-git.v4/plumbing/transport/ssh"

	"github.com/picostack/pico/executor"
	"github.com/picostack/pico/reconfigurer"
	"github.com/picostack/pico/secret"
	"github.com/picostack/pico/secret/memory"
	"github.com/picostack/pico/secret/vault"
	"github.com/picostack/pico/task"
	"github.com/picostack/pico/watcher"
)

// Config specifies static configuration parameters (from CLI or environment)
type Config struct {
	Target          task.Repo
	Hostname        string
	SSH             bool
	Directory       string
	PassEnvironment bool
	CheckInterval   time.Duration
	VaultAddress    string
	VaultToken      string `json:"-"`
	VaultPath       string
	VaultRenewal    time.Duration
	VaultConfig     string
}

// App stores application state
type App struct {
	config       Config
	reconfigurer reconfigurer.Provider
	watcher      watcher.Watcher
	secrets      secret.Store
	bus          chan task.ExecutionTask
}

// Initialise prepares an instance of the app to run
func Initialise(c Config) (app *App, err error) {
	app = new(App)

	app.config = c

	var secretStore secret.Store
	if c.VaultAddress != "" {
		zap.L().Debug("connecting to vault",
			zap.String("address", c.VaultAddress),
			zap.String("path", c.VaultPath),
			zap.String("token", c.VaultToken),
			zap.Duration("renewal", c.VaultRenewal))

		secretStore, err = vault.New(c.VaultAddress, c.VaultPath, c.VaultToken, c.VaultRenewal)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create vault secret store")
		}
	} else {
		secretStore = &memory.MemorySecrets{
			// TODO: pull env vars with PICO_SECRET_* or something and shove em here
		}
	}

	secretConfig, err := secretStore.GetSecretsForTarget(c.VaultConfig)
	if err != nil {
		zap.L().Info("could not read additional config from vault", zap.String("path", c.VaultConfig))
		err = nil
	}
	zap.L().Debug("read configuration secrets from secret store", zap.Strings("keys", getKeys(secretConfig)))

	authMethod, err := getAuthMethod(c, secretConfig)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create an authentication method from the given config")
	}

	app.secrets = secretStore

	app.bus = make(chan task.ExecutionTask, 100)

	// reconfigurer
	app.reconfigurer = reconfigurer.New(
		c.Directory,
		c.Hostname,
		c.Target.URL,
		c.CheckInterval,
		authMethod,
	)

	// target watcher
	app.watcher = watcher.NewGitWatcher(
		app.config.Directory,
		app.bus,
		app.config.CheckInterval,
		secretStore,
	)

	return
}

// Start launches the app and blocks until fatal error
func (app *App) Start(ctx context.Context) error {
	errs := make(chan error)

	ce := executor.NewCommandExecutor(app.secrets, app.config.PassEnvironment, app.config.VaultConfig, "GLOBAL_")
	go func() {
		ce.Subscribe(app.bus)
	}()

	gw := app.watcher.(*watcher.GitWatcher)
	go func() {
		errs <- errors.Wrap(
			gw.Start(),
			"git watcher crashed",
		)
	}()

	go func() {
		errs <- errors.Wrap(
			app.reconfigurer.Configure(app.watcher),
			"reconfigure provider crashed",
		)
	}()

	if s, ok := app.secrets.(*vault.VaultSecrets); ok {
		go func() {
			errs <- errors.Wrap(
				retrier.New(retrier.ConstantBackoff(3, 100*time.Millisecond), nil).RunCtx(ctx, s.Renew),
				"vault token renewal job failed",
			)
		}()
	}

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		return context.Canceled
	}
}

func getAuthMethod(c Config, secretConfig map[string]string) (transport.AuthMethod, error) {
	if c.SSH {
		authMethod, err := ssh.NewSSHAgentAuth("git")
		if err != nil {
			return nil, errors.Wrap(err, "failed to set up SSH authentication")
		}
		return authMethod, nil
	}

	if c.Target.User != "" && c.Target.Pass != "" {
		return &http.BasicAuth{
			Username: c.Target.User,
			Password: c.Target.Pass,
		}, nil
	}

	user, userok := secretConfig["GIT_USERNAME"]
	pass, passok := secretConfig["GIT_PASSWORD"]
	if userok && passok {
		return &http.BasicAuth{
			Username: user,
			Password: pass,
		}, nil
	}

	return nil, nil
}

func getKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
