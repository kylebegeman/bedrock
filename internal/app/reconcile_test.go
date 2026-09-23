package app

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/kylebegeman/bedrock/internal/docker"
	"github.com/kylebegeman/bedrock/internal/state"
)

// jobFixture is Docker as job runs see it: containers by name, with the
// exit code each running one will end with.
type jobFixture struct {
	containers []docker.Info
	exits      map[string]int
	mu         sync.Mutex // adopted runs settle from their own goroutines
	removed    []string
}

func (f *jobFixture) Owned(context.Context) ([]docker.Info, error) { return f.containers, nil }
func (f *jobFixture) Inspect(_ context.Context, name string) (*docker.Info, error) {
	for i := range f.containers {
		if f.containers[i].Name == name {
			return &f.containers[i], nil
		}
	}
	return nil, errors.New("no such container")
}
func (f *jobFixture) WaitExit(ctx context.Context, name string) (int, error) {
	if code, ok := f.exits[name]; ok {
		return code, nil
	}
	<-ctx.Done()
	return -1, ctx.Err()
}
func (f *jobFixture) LogTail(context.Context, string, int) string { return "last words\n" }
func (f *jobFixture) Remove(_ context.Context, name string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, name)
	return nil
}

func TestJobRunsADeadDaemonLeftOpenAreSettledAtStart(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	t0 := time.Date(2026, 9, 22, 3, 0, 0, 0, time.UTC)
	start := func(workload, container string) int64 {
		t.Helper()
		id, err := store.StartJobRun(ctx, state.JobRun{App: "test", Workload: workload, Revision: "r1", Kind: JobCron, StartedAt: t0, Container: container})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	running := start("report", "bedrock-test-report-job-1")
	exited := start("report", "bedrock-test-report-job-2")
	gone := start("report", "bedrock-test-report-job-3")
	unnamed := start("nightly", "")
	done := start("report", "bedrock-test-report-job-5")
	if err := store.FinishJobRun(ctx, done, 0, "", "", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	labels := func(workload string) map[string]string {
		return map[string]string{docker.LabelApp: "test", docker.LabelWorkload: workload}
	}
	e := &jobFixture{
		containers: []docker.Info{
			{Name: "bedrock-test-report-job-1", Running: true, Labels: labels("report")},
			{Name: "bedrock-test-report-job-2", Running: false, ExitCode: 0, Labels: labels("report")},
			{Name: "bedrock-test-nightly-job-4", Running: true, Labels: labels("nightly")},
		},
		exits: map[string]int{"bedrock-test-report-job-1": 3, "bedrock-test-nightly-job-4": 0},
	}
	now := t0.Add(time.Hour)
	adopted, err := reconcileJobRuns(ctx, store, e, mustOpen(t, store), now, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	adopted.Wait()
	runs := map[int64]state.JobRun{}
	for _, r := range mustRuns(t, store) {
		runs[r.ID] = r
	}
	if r := runs[running]; !r.FinishedAt.After(t0) || r.ExitCode != 3 || r.Error != "exited with code 3" || r.Output != "last words\n" {
		t.Fatalf("the adopted run: %+v", r)
	}
	if r := runs[exited]; !r.FinishedAt.Equal(now) || r.ExitCode != 0 || r.Error != "" {
		t.Fatalf("the exited run: %+v", r)
	}
	if r := runs[gone]; !r.FinishedAt.Equal(now) || r.ExitCode != -1 || r.Error != "interrupted by a daemon restart" {
		t.Fatalf("the run whose container is gone: %+v", r)
	}
	if r := runs[unnamed]; !r.FinishedAt.After(t0) || r.ExitCode != 0 || r.Error != "" {
		t.Fatalf("the unnamed run found by its labels: %+v", r)
	}
	if r := runs[done]; !r.FinishedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("a finished run was touched: %+v", r)
	}
	slices.Sort(e.removed)
	if want := []string{"bedrock-test-nightly-job-4", "bedrock-test-report-job-1", "bedrock-test-report-job-2"}; !slices.Equal(e.removed, want) {
		t.Fatalf("removed %v, want %v", e.removed, want)
	}
	if open := mustOpen(t, store); len(open) != 0 {
		t.Fatalf("still open: %+v", open)
	}
}

func mustOpen(t *testing.T, store *state.Store) []state.JobRun {
	t.Helper()
	open, err := store.OpenJobRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return open
}

func mustRuns(t *testing.T, store *state.Store) []state.JobRun {
	t.Helper()
	runs, err := store.JobRuns(context.Background(), "test", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	return runs
}
