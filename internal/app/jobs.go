package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/kylebegeman/quark/internal/docker"
	"github.com/kylebegeman/quark/internal/kernel"
	"github.com/kylebegeman/quark/internal/manifest"
	"github.com/kylebegeman/quark/internal/secrets"
	"github.com/kylebegeman/quark/internal/state"
)

// RunKind runs a one-off command in a workload's environment.
const RunKind = "app.run"

// Job kinds as job_runs records them.
const (
	JobCron = "cron"
	JobRun  = "run"
)

// Jobs runs cron workloads and one-off commands.
type Jobs struct {
	Store   *state.Store
	Secrets *secrets.Store
	mu      sync.Mutex
	running map[string]bool
}

// NewJobs returns a job runner.
func NewJobs(store *state.Store, sec *secrets.Store) *Jobs {
	return &Jobs{Store: store, Secrets: sec, running: map[string]bool{}}
}

// Run executes one job: a container from the workload's image with the
// revision's environment, secrets and mounts, waited for and removed.
// Output streams to out; the last lines and the exit code are recorded.
func (j *Jobs) Run(ctx context.Context, rev *state.Revision, workload string, command []string, kind string, out io.Writer) (int, error) {
	var m manifest.Manifest
	if err := json.Unmarshal(rev.Manifest, &m); err != nil {
		return -1, err
	}
	w, ok := m.Workloads[workload]
	if !ok {
		return -1, fmt.Errorf("%s has no workload named %s", rev.App, workload)
	}
	image := rev.Images[workload]
	if image == "" {
		return -1, fmt.Errorf("no image recorded for %s", workload)
	}
	e, err := docker.Connect(ctx)
	if err != nil {
		return -1, err
	}
	defer e.Close()
	values, err := j.Secrets.Load(rev.App, rev.SecretsVersion)
	if err != nil {
		return -1, err
	}
	spec, err := containerSpec(&m, workload, w, rev.ID, image, values)
	if err != nil {
		return -1, err
	}
	spec.Name = fmt.Sprintf("quark-%s-%s-job-%d", rev.App, workload, time.Now().UnixMilli())
	spec.Restart = false
	// A job never takes traffic: it stays on the app's own network.
	spec.Networks = []string{docker.AppNetwork(rev.App)}
	if len(command) > 0 {
		spec.Cmd = command
	}
	if _, err := isolate(ctx, e, &spec, w, image); err != nil {
		return -1, err
	}
	if kind == JobRun {
		// A one-off command is the operator's: it may write where it
		// likes, and is gone when it ends.
		spec.ReadOnly = false
	}
	started := time.Now().UTC()
	id, err := j.Store.StartJobRun(ctx, state.JobRun{App: rev.App, Workload: workload, Revision: rev.ID, Kind: kind, StartedAt: started})
	if err != nil {
		return -1, err
	}
	timeout := time.Hour
	if w.Timeout != "" {
		if d, err := time.ParseDuration(w.Timeout); err == nil {
			timeout = d
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	code, runErr := j.execute(runCtx, e, spec, out)
	// Removal and the record use a fresh context: the job's may have ended.
	cleanup, cancelCleanup := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelCleanup()
	output := e.LogTail(cleanup, spec.Name, 40)
	_ = e.Remove(cleanup, spec.Name, 5*time.Second)
	errText := ""
	if runErr != nil {
		errText = runErr.Error()
	}
	if err := j.Store.FinishJobRun(cleanup, id, code, errText, output, time.Now().UTC()); err != nil {
		return code, err
	}
	return code, runErr
}

func (j *Jobs) execute(ctx context.Context, e *docker.Engine, spec docker.Spec, out io.Writer) (int, error) {
	if err := e.Run(ctx, spec); err != nil {
		return -1, err
	}
	// The log copy may outlive the wait by a moment; once the job is done
	// it writes nowhere, so the step's output is never written after it ends.
	guarded := &closingWriter{w: out}
	defer guarded.close()
	logs := make(chan error, 1)
	go func() { logs <- e.Logs(ctx, spec.Name, true, "all", guarded) }()
	code, err := e.WaitExit(ctx, spec.Name)
	if errors.Is(err, context.DeadlineExceeded) {
		_ = e.Remove(context.Background(), spec.Name, time.Second)
		return -1, errors.New("timed out")
	}
	if err != nil {
		return -1, err
	}
	select {
	case <-logs:
	case <-time.After(3 * time.Second):
	}
	if code != 0 {
		return code, fmt.Errorf("exited with code %d", code)
	}
	return 0, nil
}

// Tick runs every cron workload of every active revision that is due.
// Each workload runs one at a time; a run still going when the next is
// due is skipped.
func (j *Jobs) Tick(ctx context.Context, now time.Time, log func(string, ...any)) {
	active, err := j.Store.ActiveRevisions(ctx)
	if err != nil {
		log("cron: %v", err)
		return
	}
	for _, rev := range active {
		var m manifest.Manifest
		if err := json.Unmarshal(rev.Manifest, &m); err != nil {
			continue
		}
		for _, name := range m.WorkloadNames() {
			w := m.Workloads[name]
			if w.Kind != manifest.Cron {
				continue
			}
			sched, err := manifest.ParseSchedule(w.Schedule)
			if err != nil {
				continue
			}
			last, err := j.Store.LastJobRun(ctx, rev.App, name)
			if err != nil {
				log("cron: %v", err)
				continue
			}
			since := rev.CreatedAt
			if last != nil && last.StartedAt.After(since) {
				since = last.StartedAt
			}
			if sched.Next(since).After(now) {
				continue
			}
			key := rev.App + "/" + name
			j.mu.Lock()
			busy := j.running[key]
			if !busy {
				j.running[key] = true
			}
			j.mu.Unlock()
			if busy {
				continue
			}
			rev := rev
			go func() {
				defer func() {
					j.mu.Lock()
					delete(j.running, key)
					j.mu.Unlock()
				}()
				code, err := j.Run(context.Background(), &rev, name, nil, JobCron, io.Discard)
				if err != nil {
					log("cron %s: %v (exit %d)", key, err, code)
					return
				}
				log("cron %s: ok", key)
			}()
		}
	}
}

// RunDefinition is the Definition for RunKind.
type RunDefinition struct {
	Jobs *Jobs
}

// RunInput says what to run.
type RunInput struct {
	App      string   `json:"app"`
	Workload string   `json:"workload,omitempty"`
	Command  []string `json:"command"`
}

// Kind implements kernel.Definition.
func (RunDefinition) Kind() string { return RunKind }

// Plan implements kernel.Definition.
func (r RunDefinition) Plan(ctx context.Context, raw json.RawMessage) (*kernel.Plan, error) {
	var in RunInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("run input: %w", err)
	}
	if len(in.Command) == 0 {
		return nil, errors.New("nothing to run")
	}
	rev, err := r.Jobs.Store.RevisionWithStatus(ctx, in.App, state.RevisionActive)
	if err != nil {
		return nil, err
	}
	if rev == nil {
		return nil, fmt.Errorf("%s isn't deployed", in.App)
	}
	var m manifest.Manifest
	if err := json.Unmarshal(rev.Manifest, &m); err != nil {
		return nil, err
	}
	workload := in.Workload
	if workload == "" {
		names := m.WorkloadNames()
		if len(names) != 1 {
			return nil, fmt.Errorf("%s has several workloads; name one of %s", in.App, strings.Join(names, ", "))
		}
		workload = names[0]
	}
	if _, ok := m.Workloads[workload]; !ok {
		return nil, fmt.Errorf("%s has no workload named %s", in.App, workload)
	}
	return &kernel.Plan{Target: in.App + " " + workload, Recovery: kernel.Resume, Steps: []kernel.Step{{
		Name: "run", Change: fmt.Sprintf("run %s in %s's environment", strings.Join(in.Command, " "), workload),
		Note: "revision " + rev.ID,
		Apply: func(ctx context.Context, out io.Writer) error {
			_, err := r.Jobs.Run(ctx, rev, workload, in.Command, JobRun, out)
			return err
		},
	}}}, nil
}

// closingWriter forwards writes until it is closed, then drops them.
type closingWriter struct {
	mu     sync.Mutex
	w      io.Writer
	closed bool
}

func (c *closingWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return len(p), nil
	}
	return c.w.Write(p)
}

func (c *closingWriter) close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
}
