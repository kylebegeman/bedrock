// Package daemon is bedrock on a machine it manages: it owns the state store,
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
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/kylebegeman/bedrock/internal/api"
	"github.com/kylebegeman/bedrock/internal/app"
	"github.com/kylebegeman/bedrock/internal/docker"
	"github.com/kylebegeman/bedrock/internal/edge"
	"github.com/kylebegeman/bedrock/internal/gitdeploy"
	"github.com/kylebegeman/bedrock/internal/host"
	"github.com/kylebegeman/bedrock/internal/kernel"
	"github.com/kylebegeman/bedrock/internal/manifest"
	"github.com/kylebegeman/bedrock/internal/secrets"
	"github.com/kylebegeman/bedrock/internal/signals"
	"github.com/kylebegeman/bedrock/internal/state"
	"github.com/kylebegeman/bedrock/internal/version"
	"github.com/kylebegeman/bedrock/internal/watch"
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
const DefaultSocket = "/run/bedrock/bedrock.sock"

// DefaultStateDir is where the daemon keeps its store on a machine it manages.
const DefaultStateDir = "/var/lib/bedrock"

// Registry returns the operation kinds the daemon knows, for the machine
// this process runs on, keeping its state in stateDir.
func Registry(store *state.Store, sec *secrets.Store, socket, stateDir string) kernel.Registry {
	env := host.RealEnv()
	reg := kernel.Registry{}
	reg.Add(kernel.Exercise{})
	cloudflareOnly := app.CloudflareOnly(func() (bool, error) {
		p, err := host.LoadProfile(env)
		if errors.Is(err, host.ErrNoProfile) {
			return false, nil
		}
		return p.CloudflareOnly(), err
	})
	reg.Add(host.Setup{Env: env, Socket: socket, Routes: func(ctx context.Context) ([]host.Route, error) {
		return routesOf(ctx, store)
	}, EnsureEdge: func(ctx context.Context, out io.Writer) error {
		e, err := docker.Connect(ctx)
		if err != nil {
			return err
		}
		defer e.Close()
		boot, err := app.EdgeConfig(ctx, store, sec)
		if err != nil {
			return err
		}
		if err := edge.Ensure(ctx, e, boot, out); err != nil {
			return err
		}
		return app.ReloadEdge(ctx, store, sec)
	}})
	reg.Add(host.Maintain{Env: env, Socket: socket})
	reg.Add(host.Upgrade{Env: env, Socket: socket})
	deploy := app.Deploy{Store: store, Secrets: sec, StateDir: stateDir, Addresses: func(ctx context.Context) []string { return host.Addresses(ctx, env) }, CloudflareOnly: cloudflareOnly}
	reg.Add(deploy)
	reg.Add(app.Rollback{Deploy: deploy})
	reg.Add(app.GC{Store: store})
	reg.Add(app.RunDefinition{Jobs: app.NewJobs(store, sec)})
	addresses := func(ctx context.Context) []string { return host.Addresses(ctx, env) }
	reg.Add(app.Remove{Store: store, Secrets: sec, Addresses: addresses, StateDir: stateDir})
	reg.Add(app.Point{Store: store, Secrets: sec, Addresses: addresses, CloudflareOnly: cloudflareOnly})
	reg.Add(app.Drop{Store: store, Secrets: sec, Addresses: addresses})
	reg.Add(app.Backup{Store: store, Secrets: sec, StateDir: stateDir, Hostname: hostname, Profile: host.ProfilePath})
	reg.Add(app.Drill{Store: store, Secrets: sec, StateDir: stateDir})
	reg.Add(app.RestoreDef{Store: store, Secrets: sec, StateDir: stateDir})
	return reg
}

// routesOf lists what the machine's apps route, as host setup needs it.
func routesOf(ctx context.Context, store *state.Store) ([]host.Route, error) {
	routes, err := app.Routes(ctx, store)
	if err != nil {
		return nil, err
	}
	out := make([]host.Route, 0, len(routes))
	for _, r := range routes {
		out = append(out, host.Route{App: r.App, Host: r.Host, Mode: string(r.Mode), Proxied: r.Mode == manifest.DNSProxied})
	}
	return out, nil
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "bedrock"
	}
	return h
}

// Run serves until ctx ends. Log lines go to logw.
func Run(ctx context.Context, cfg Config, logw io.Writer) error {
	cfg = cfg.withDefaults()
	logf := lineLogger(logw)
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
	if err := app.RecoverBackupPauses(ctx, store.Path()); err != nil {
		return fmt.Errorf("recover backup pauses: %w", err)
	}
	// Job runs the last daemon left open are taken up again or closed;
	// Docker being down only means they wait for the next start.
	if err := app.ReconcileJobRuns(ctx, store, logf); err != nil {
		logf("job runs: %v", err)
	}
	clearStaleUpgrade(logf)
	sec := secrets.DefaultStore(cfg.StateDir)
	engine := kernel.New(store, Registry(store, sec, cfg.Socket, cfg.StateDir), cfg.Owner)
	logf("bedrock daemon %s, state %s, owner %s", version.Current().Version, store.Path(), cfg.Owner)
	d := &daemon{cfg: cfg, logf: logf, store: store, sec: sec, api: &api.Server{Engine: engine, Store: store}, machine: hostname()}

	if err := d.recoverOperations(ctx); err != nil {
		return err
	}
	// The edge gets the configuration this build of bedrock makes for the
	// active revisions, in case the shape changed since the last deploy;
	// an edge an older bedrock made is replaced, keeping its routes.
	if err := app.UpgradeEdge(ctx, store, sec, logWriter{logf}); err != nil {
		logf("edge: %v", err)
	}
	d.startLoops(ctx)
	receiver, stopHooks := d.serveHooks()
	defer stopHooks()
	return d.serveAPI(ctx, receiver)
}

// withDefaults fills in what a Config leaves out.
func (cfg Config) withDefaults() Config {
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
	return cfg
}

// lineLogger writes whole lines to w, one at a time: many loops log.
func lineLogger(w io.Writer) func(string, ...any) {
	var mu sync.Mutex
	return func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, format+"\n", args...)
	}
}

// daemon is a running daemon: what its loops and servers share.
type daemon struct {
	cfg     Config
	logf    func(string, ...any)
	store   *state.Store
	sec     *secrets.Store
	api     *api.Server
	machine string
}

// recoverOperations finishes what a previous daemon left mid-way with an
// expired lease, then keeps sweeping: a lease still live at start belongs
// to a daemon that died moments ago, and is picked up when it expires.
func (d *daemon) recoverOperations(ctx context.Context) error {
	logf := d.logf
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
	if recovered, err := d.api.Recover(ctx, recoveryLog); err != nil {
		return fmt.Errorf("recover: %w", err)
	} else if recovered > 0 {
		logf("recovered %d interrupted operation(s)", recovered)
	}
	go every(ctx, d.cfg.SweepInterval, false, func(time.Time) {
		guarded(logf, "recovery sweep", func() {
			if n, err := d.api.Recover(ctx, recoveryLog); err != nil && ctx.Err() == nil {
				logf("recovery sweep: %v", err)
			} else if n > 0 {
				logf("recovered %d interrupted operation(s)", n)
			}
		})
	})
	return nil
}

// startLoops starts what runs on the daemon's own clocks: cron workloads,
// outside the operation lock; watching; signals; and scheduled backups,
// drills and collection.
func (d *daemon) startLoops(ctx context.Context) {
	logf := d.logf
	jobs := app.NewJobs(d.store, d.sec)
	go every(ctx, 30*time.Second, false, func(now time.Time) {
		guarded(logf, "cron", func() { jobs.Tick(ctx, now, logf) })
	})
	watcher := &watch.Watcher{Store: d.store, Notifier: watch.EmailNotifier{Secrets: d.sec, Store: d.store}, Hostname: d.machine, Log: logf}
	prober := watch.NewProber(d.store)
	go every(ctx, time.Minute, true, func(time.Time) {
		guarded(logf, "watch", func() {
			if err := watcher.Round(ctx, prober.Observe(ctx)); err != nil && ctx.Err() == nil {
				logf("watch: %v", err)
			}
		})
	})
	sampler := signals.NewSampler(d.store)
	sampler.Log = logf
	go every(ctx, time.Minute, true, func(time.Time) {
		guarded(logf, "signals", func() {
			if err := sampler.Sample(ctx); err != nil && ctx.Err() == nil {
				logf("signals: %v", err)
			}
		})
	})
	scheduler := &Scheduler{Store: d.store, Secrets: d.sec, Server: d.api, Log: logf, Started: time.Now().UTC()}
	go every(ctx, 30*time.Second, false, func(now time.Time) {
		guarded(logf, "schedule", func() { scheduler.Tick(ctx, now) })
	})
}

// serveHooks answers GitHub's webhooks, which reach the daemon through the
// edge on a socket in the directory the two share. It returns the
// receiver, to wait for its deploys at shutdown, and what closes the
// server. A socket that can't be opened is logged, and webhooks wait.
func (d *daemon) serveHooks() (*gitdeploy.Receiver, func()) {
	receiver := &gitdeploy.Receiver{
		Store: d.store, Secrets: d.sec,
		Deploy: gitdeploy.InProcess(d.api.RunLocked),
		Notify: func(ctx context.Context, subject, body string) {
			if err := (watch.EmailNotifier{Secrets: d.sec, Store: d.store}).Notify(ctx, watch.Notice{Subject: d.machine + ": " + subject, Body: body}); err != nil {
				d.logf("webhooks: telling you about %s failed: %v", subject, err)
			}
		},
		Log:        d.logf,
		SourcesDir: filepath.Join(d.cfg.StateDir, "sources"),
		BuildsDir:  filepath.Join(d.cfg.StateDir, "builds"),
	}
	hooks, err := listenHooks()
	if err != nil {
		d.logf("webhooks: %v", err)
		return receiver, func() {}
	}
	// The edge puts this on the internet, so every read is bounded.
	hookServer := &http.Server{Handler: receiver, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute}
	go func() { _ = hookServer.Serve(hooks) }()
	return receiver, func() { hookServer.Close() }
}

// serveAPI serves the API on the daemon's socket, tells systemd it is
// ready and keeps its watchdog fed, and shuts down when ctx ends: the API
// first, then the deploys webhooks started, given a while to finish.
func (d *daemon) serveAPI(ctx context.Context, receiver *gitdeploy.Receiver) error {
	listener, err := api.Listen(d.cfg.Socket)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: d.api.Handler(), ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	d.logf("listening on %s", d.cfg.Socket)

	notifier := notifierFromEnv()
	notifier.ready()
	watchdog := notifier.watchdog(ctx, func() bool {
		// Feed the watchdog only while the store answers; a wedged daemon
		// gets restarted by systemd instead of lingering.
		_, err := d.store.List(ctx, 1)
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
	// A deploy a webhook started is journaled and would be resumed, but
	// finishing it here is better than resuming it there.
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelWait()
	receiver.Wait(waitCtx)
	d.logf("stopped")
	return nil
}

// guarded runs fn and turns a panic in it into a log line with its stack,
// so one loop's bug doesn't take the daemon down with every other loop.
func guarded(logf func(string, ...any), name string, fn func()) {
	defer func() {
		if p := recover(); p != nil {
			logf("%s: panic: %v\n%s", name, p, debug.Stack())
		}
	}()
	fn()
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

// clearStaleUpgrade removes an upgrade marker too old to belong to an
// upgrade in flight, so the rollback unit never acts on it; an upgrade
// that verified removes its own.
func clearStaleUpgrade(logf func(string, ...any)) {
	if info, err := os.Stat(host.StagedMarker); err == nil && time.Since(info.ModTime()) > 10*time.Minute {
		if os.Remove(host.StagedMarker) == nil {
			logf("cleared a stale upgrade marker from %s", info.ModTime().UTC().Format(time.RFC3339))
		}
	}
}
