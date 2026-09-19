// Package daemon is quark on a machine it manages: it owns the state store,
// recovers interrupted operations at start, serves the API on a Unix socket,
// and keeps systemd's watchdog fed from its own loop.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kylebegeman/quark/internal/api"
	"github.com/kylebegeman/quark/internal/app"
	"github.com/kylebegeman/quark/internal/docker"
	"github.com/kylebegeman/quark/internal/edge"
	"github.com/kylebegeman/quark/internal/gitdeploy"
	"github.com/kylebegeman/quark/internal/host"
	"github.com/kylebegeman/quark/internal/kernel"
	"github.com/kylebegeman/quark/internal/secrets"
	"github.com/kylebegeman/quark/internal/signals"
	"github.com/kylebegeman/quark/internal/state"
	"github.com/kylebegeman/quark/internal/version"
	"github.com/kylebegeman/quark/internal/watch"
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
func Registry(store *state.Store, sec *secrets.Store, socket string) kernel.Registry {
	return registry(store, sec, socket, DefaultStateDir)
}

// RegistryIn is Registry for a state directory other than the default.
func RegistryIn(store *state.Store, sec *secrets.Store, socket, stateDir string) kernel.Registry {
	return registry(store, sec, socket, stateDir)
}

func registry(store *state.Store, sec *secrets.Store, socket, stateDir string) kernel.Registry {
	env := host.RealEnv()
	reg := kernel.Registry{}
	reg.Add(kernel.Exercise{})
	reg.Add(host.Setup{Env: env, Socket: socket, EnsureEdge: func(ctx context.Context, out io.Writer) error {
		e, err := docker.Connect(ctx)
		if err != nil {
			return err
		}
		defer e.Close()
		boot, err := app.EdgeConfig(ctx, store)
		if err != nil {
			return err
		}
		if err := edge.Ensure(ctx, e, boot, out); err != nil {
			return err
		}
		return app.ReloadEdge(ctx, store)
	}})
	reg.Add(host.Maintain{Env: env, Socket: socket})
	reg.Add(host.Upgrade{Env: env})
	deploy := app.Deploy{Store: store, Secrets: sec, Addresses: func(ctx context.Context) []string { return host.Addresses(ctx, env) }}
	reg.Add(deploy)
	reg.Add(app.Rollback{Deploy: deploy})
	reg.Add(app.GC{Store: store})
	reg.Add(app.RunDefinition{Jobs: app.NewJobs(store, sec)})
	addresses := func(ctx context.Context) []string { return host.Addresses(ctx, env) }
	reg.Add(app.Remove{Store: store, Secrets: sec, Addresses: addresses})
	reg.Add(app.Point{Store: store, Secrets: sec, Addresses: addresses})
	reg.Add(app.Backup{Store: store, Secrets: sec, StateDir: stateDir, Hostname: hostname})
	reg.Add(app.Drill{Store: store, Secrets: sec, StateDir: stateDir})
	reg.Add(app.RestoreDef{Store: store, Secrets: sec, StateDir: stateDir})
	return reg
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "quark"
	}
	return h
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
	sec := secrets.DefaultStore(cfg.StateDir)
	engine := kernel.New(store, RegistryIn(store, sec, cfg.Socket, cfg.StateDir), cfg.Owner)
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

	// The edge gets the configuration this build of quark makes for the
	// active revisions, in case the shape changed since the last deploy;
	// an edge an older quark made is replaced, keeping its routes.
	if err := app.UpgradeEdge(ctx, store, logWriter{logf}); err != nil {
		logf("edge: %v", err)
	}

	// Cron workloads run on the daemon's clock, outside the operation lock.
	jobs := app.NewJobs(store, sec)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				jobs.Tick(ctx, now.UTC(), logf)
			}
		}
	}()

	// Watching, signals and scheduled backups run on their own clocks.
	machine := hostname()
	watcher := &watch.Watcher{Store: store, Notifier: watch.EmailNotifier{Secrets: sec, Store: store}, Hostname: machine, Log: logf}
	prober := watch.NewProber(store)
	go every(ctx, time.Minute, true, func(now time.Time) {
		if err := watcher.Round(ctx, prober.Observe(ctx)); err != nil && ctx.Err() == nil {
			logf("watch: %v", err)
		}
	})
	sampler := signals.NewSampler(store)
	sampler.Log = logf
	go every(ctx, time.Minute, true, func(now time.Time) {
		if err := sampler.Sample(ctx); err != nil && ctx.Err() == nil {
			logf("signals: %v", err)
		}
	})
	scheduler := &Scheduler{Store: store, Secrets: sec, Server: apiServer, Log: logf, Started: time.Now().UTC()}
	go every(ctx, 30*time.Second, false, func(now time.Time) { scheduler.Tick(ctx, now) })

	// GitHub's webhooks reach the daemon through the edge, on a socket in
	// the directory the two share.
	receiver := &gitdeploy.Receiver{
		Store: store, Secrets: sec,
		Deploy: gitdeploy.InProcess(apiServer.RunLocked),
		Notify: func(ctx context.Context, subject, body string) {
			_ = watch.EmailNotifier{Secrets: sec, Store: store}.Notify(ctx, watch.Notice{Subject: machine + ": " + subject, Body: body})
		},
		Log:        logf,
		SourcesDir: filepath.Join(cfg.StateDir, "sources"),
		BuildsDir:  filepath.Join(cfg.StateDir, "builds"),
	}
	if hooks, err := listenHooks(); err != nil {
		logf("webhooks: %v", err)
	} else {
		hookServer := &http.Server{Handler: receiver, ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = hookServer.Serve(hooks) }()
		defer hookServer.Close()
	}

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

// every calls fn on a period until ctx ends, first right away when asked.
func every(ctx context.Context, period time.Duration, now bool, fn func(time.Time)) {
	if now {
		fn(time.Now().UTC())
	}
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			fn(t.UTC())
		}
	}
}

// logWriter turns lines written to it into log lines.
type logWriter struct{ logf func(string, ...any) }

func (w logWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line != "" {
			w.logf("%s", line)
		}
	}
	return len(p), nil
}

// listenHooks opens the webhooks socket the edge proxies to.
func listenHooks() (net.Listener, error) {
	if err := os.MkdirAll(edge.RunDir, 0o755); err != nil {
		return nil, err
	}
	_ = os.Remove(edge.HooksSocket)
	l, err := net.Listen("unix", edge.HooksSocket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(edge.HooksSocket, 0o660); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}
