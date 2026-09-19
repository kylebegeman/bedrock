// Package daemon is quark on a machine it manages: it owns the state store,
// recovers interrupted operations at start, serves the API on a Unix socket,
// and keeps systemd's watchdog fed from its own loop.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kylebegeman/quark/internal/api"
	"github.com/kylebegeman/quark/internal/host"
	"github.com/kylebegeman/quark/internal/kernel"
	"github.com/kylebegeman/quark/internal/state"
	"github.com/kylebegeman/quark/internal/version"
)

// Config says where the daemon keeps things.
type Config struct {
	StateDir string
	Socket   string
	// Owner names this process in leases. Defaults to host-pid.
	Owner string
	// SweepInterval is how often the daemon looks for operations whose
	// daemon died. Defaults to 5 seconds.
	SweepInterval time.Duration
}

// DefaultSocket is where the daemon listens on a machine it manages.
const DefaultSocket = "/run/quark/quark.sock"

// DefaultStateDir is where the daemon keeps its store on a machine it manages.
const DefaultStateDir = "/var/lib/quark"

// Registry returns the operation kinds the daemon knows, for the machine
// this process runs on.
func Registry(socket string) kernel.Registry {
	env := host.RealEnv()
	reg := kernel.Registry{}
	reg.Add(kernel.Exercise{})
	reg.Add(host.Setup{Env: env, Socket: socket})
	reg.Add(host.Maintain{Env: env, Socket: socket})
	reg.Add(host.Upgrade{Env: env})
	return reg
}

// Run serves until ctx ends. Log lines go to logw.
func Run(ctx context.Context, cfg Config, logw io.Writer) error {
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir
	}
	if cfg.Socket == "" {
		cfg.Socket = DefaultSocket
	}
	if cfg.Owner == "" {
		host, _ := os.Hostname()
		cfg.Owner = fmt.Sprintf("%s-%d", host, os.Getpid())
	}
	if cfg.SweepInterval <= 0 {
		cfg.SweepInterval = 5 * time.Second
	}
	logf := func(format string, args ...any) { fmt.Fprintf(logw, format+"\n", args...) }
	// A build whose version says "-broken" is the lane's fixture for a bad
	// upgrade: it must fail to start so systemd rolls the binary back.
	if strings.Contains(version.Current().Version, "-broken") {
		return errors.New("this build is deliberately broken (a lane fixture) and refuses to start")
	}

	store, err := state.Open(filepath.Join(cfg.StateDir, "state.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	engine := kernel.New(store, Registry(cfg.Socket), cfg.Owner)
	logf("quark daemon %s, state %s, owner %s", version.Current().Version, store.Path(), cfg.Owner)

	apiServer := &api.Server{Engine: engine, Store: store}
	recoveryLog := func(ev kernel.Event) {
		switch ev.Type {
		case kernel.EventResumed:
			logf("recovering %s: %s", ev.Operation, ev.Message)
		case kernel.EventFinished:
			if ev.Receipt != nil {
				logf("recovered %s: %s", ev.Operation, ev.Receipt.Status)
			}
		}
	}
	// Whatever a previous daemon left mid-way with an expired lease is
	// finished first. A lease that is still live at this point belongs to
	// a daemon that died moments ago; the sweep below picks it up when it
	// expires.
	if recovered, err := apiServer.Recover(ctx, recoveryLog); err != nil {
		return fmt.Errorf("recover: %w", err)
	} else if recovered > 0 {
		logf("recovered %d interrupted operation(s)", recovered)
	}
	go func() {
		ticker := time.NewTicker(cfg.SweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if n, err := apiServer.Recover(ctx, recoveryLog); err != nil && ctx.Err() == nil {
					logf("recovery sweep: %v", err)
				} else if n > 0 {
					logf("recovered %d interrupted operation(s)", n)
				}
			}
		}
	}()

	listener, err := api.Listen(cfg.Socket)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: apiServer.Handler(), ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	logf("listening on %s", cfg.Socket)

	notifier := notifierFromEnv()
	notifier.ready()
	watchdog := notifier.watchdog(ctx, func() bool {
		// Feed the watchdog only while the store answers; a wedged daemon
		// gets restarted by systemd instead of lingering.
		_, err := store.List(ctx, 1)
		return err == nil
	})

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
	}
	notifier.stopping()
	<-watchdog
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	logf("stopped")
	return nil
}
