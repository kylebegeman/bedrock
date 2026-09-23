package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kylebegeman/bedrock/internal/docker"
	"github.com/kylebegeman/bedrock/internal/state"
)

// jobEngine is what settling a job run needs of Docker.
type jobEngine interface {
	Owned(context.Context) ([]docker.Info, error)
	Inspect(context.Context, string) (*docker.Info, error)
	WaitExit(context.Context, string) (int, error)
	LogTail(context.Context, string, int) string
	Remove(context.Context, string, time.Duration) error
}

// ReconcileJobRuns settles the job runs a daemon that died left open, at
// the next daemon's start. A run whose container still runs is adopted:
// waited for and recorded as if the daemon had never gone. One whose
// container has exited is recorded from what it left behind. One with no
// container is finished as interrupted, so it stops reading as running.
//
// It returns once every run has been looked at; adopted runs are waited
// for in the background, and log says what became of each.
func ReconcileJobRuns(ctx context.Context, store *state.Store, log func(string, ...any)) error {
	open, err := store.OpenJobRuns(ctx)
	if err != nil || len(open) == 0 {
		return err
	}
	e, err := docker.Connect(ctx)
	if err != nil {
		return err
	}
	adopted, err := reconcileJobRuns(ctx, store, e, open, time.Now().UTC(), log)
	go func() {
		adopted.Wait()
		e.Close()
	}()
	return err
}

func reconcileJobRuns(ctx context.Context, store *state.Store, e jobEngine, open []state.JobRun, now time.Time, log func(string, ...any)) (*sync.WaitGroup, error) {
	adopted := &sync.WaitGroup{}
	owned, err := e.Owned(ctx)
	if err != nil {
		return adopted, err
	}
	var failures []error
	for _, run := range open {
		name := run.Container
		if name == "" {
			// A run recorded before the container's name was kept.
			name = jobContainerOf(owned, run)
		}
		var info *docker.Info
		if name != "" {
			info, err = e.Inspect(ctx, name)
		}
		switch {
		case name == "" || err != nil:
			log("job %s/%s run %d: its container is gone; recorded as interrupted", run.App, run.Workload, run.ID)
			if err := store.FinishJobRun(ctx, run.ID, -1, "interrupted by a daemon restart", "", now); err != nil {
				failures = append(failures, err)
			}
		case info.Running:
			log("job %s/%s run %d: still running as %s; waiting for it", run.App, run.Workload, run.ID, name)
			adopted.Add(1)
			go func(run state.JobRun, name string) {
				defer adopted.Done()
				code, err := e.WaitExit(ctx, name)
				if err != nil {
					return // the daemon is stopping; the next one takes the run up again
				}
				settleJobRun(store, e, run, name, code, time.Now().UTC(), log)
			}(run, name)
		default:
			settleJobRun(store, e, run, name, info.ExitCode, now, log)
		}
	}
	return adopted, errors.Join(failures...)
}

// jobContainerOf finds the one running job container that carries a run's
// app and workload, for a run recorded before the container's name was
// kept. Two candidates mean neither can be called this run's.
func jobContainerOf(owned []docker.Info, run state.JobRun) string {
	name := ""
	for _, c := range owned {
		if !c.Running || !strings.Contains(c.Name, "-job-") || c.Labels[docker.LabelApp] != run.App || c.Labels[docker.LabelWorkload] != run.Workload {
			continue
		}
		if name != "" {
			return ""
		}
		name = c.Name
	}
	return name
}

// settleJobRun records how a run the daemon lost track of ended and removes
// its container, as RunWith would have.
func settleJobRun(store *state.Store, e jobEngine, run state.JobRun, name string, code int, finished time.Time, log func(string, ...any)) {
	cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output := e.LogTail(cleanup, name, 40)
	_ = e.Remove(cleanup, name, 5*time.Second)
	errText := ""
	if code != 0 {
		errText = fmt.Sprintf("exited with code %d", code)
	}
	if err := store.FinishJobRun(cleanup, run.ID, code, errText, output, finished); err != nil {
		log("job %s/%s run %d: %v", run.App, run.Workload, run.ID, err)
		return
	}
	log("job %s/%s run %d: finished after the daemon restarted (exit %d)", run.App, run.Workload, run.ID, code)
}
