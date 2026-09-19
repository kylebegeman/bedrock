package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/kylebegeman/quark/internal/docker"
	"github.com/kylebegeman/quark/internal/manifest"
)

type pauseFixture struct {
	containers          []docker.Info
	events              []string
	failStop, failStart string
	killed              bool
}

func (e *pauseFixture) Owned(context.Context) ([]docker.Info, error) { return e.containers, nil }
func (e *pauseFixture) Inspect(_ context.Context, id string) (*docker.Info, error) {
	for i := range e.containers {
		if e.containers[i].ID == id {
			return &e.containers[i], nil
		}
	}
	return nil, errors.New("missing")
}
func (e *pauseFixture) Stop(ctx context.Context, id string, _ time.Duration) error {
	e.events = append(e.events, "stop:"+id)
	if id == e.failStop {
		return errors.New("stop failed")
	}
	c, _ := e.Inspect(ctx, id)
	c.Running = false
	if e.killed {
		c.ExitCode = 137
	}
	return nil
}
func (e *pauseFixture) Start(ctx context.Context, id string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	e.events = append(e.events, "start:"+id)
	if id == e.failStart {
		return errors.New("start failed")
	}
	c, _ := e.Inspect(ctx, id)
	c.Running = true
	return nil
}
func newPauseFixture() (*pauseFixture, *manifest.Manifest) {
	e := &pauseFixture{}
	for _, v := range []struct {
		id, role string
		running  bool
	}{{"objects", objectsWorkload, true}, {"api", "api", true}, {"idle", "api", false}, {"db", postgresWorkload, true}} {
		e.containers = append(e.containers, docker.Info{ID: v.id, Name: v.id, Running: v.running, Labels: map[string]string{docker.LabelApp: "test", docker.LabelWorkload: v.role}})
	}
	return e, &manifest.Manifest{App: "test", Workloads: map[string]manifest.Workload{"api": {Kind: manifest.Web}}}
}
func TestBackupQuiescesWritersAndResumesOnCaptureFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			e, m := newPauseFixture()
			stateFile := filepath.Join(t.TempDir(), "state.db")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := withQuiescedWriters(ctx, e, stateFile, m, io.Discard, func(context.Context) error {
				if e.containers[0].Running || e.containers[1].Running || !e.containers[3].Running {
					t.Fatal("writers not quiesced or database stopped")
				}
				e.events = append(e.events, "capture")
				if fail {
					cancel()
					return context.Canceled
				}
				return nil
			})
			if (err != nil) != fail {
				t.Fatalf("capture error: %v", err)
			}
			want := []string{"stop:api", "stop:objects", "capture", "start:objects", "start:api"}
			if !slices.Equal(e.events, want) {
				t.Fatalf("events %v", e.events)
			}
			if _, err := os.Stat(quiesceFile(stateFile, m.App)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("pause record retained after successful recovery")
			}
		})
	}
}
func TestBackupRejectsUncleanStopAndRecoversPartialPause(t *testing.T) {
	for _, kind := range []string{"stop-error", "killed"} {
		t.Run(kind, func(t *testing.T) {
			e, m := newPauseFixture()
			if kind == "stop-error" {
				e.failStop = "objects"
			} else {
				e.killed = true
			}
			err := withQuiescedWriters(context.Background(), e, filepath.Join(t.TempDir(), "state.db"), m, io.Discard, func(context.Context) error { t.Fatal("captured an unsafe snapshot"); return nil })
			if err == nil || !e.containers[1].Running {
				t.Fatalf("failed recovery: %v %v", err, e.events)
			}
		})
	}
}
func TestBackupRecoveryRetainsFailedRestartsAndReplays(t *testing.T) {
	e, m := newPauseFixture()
	stateFile := filepath.Join(t.TempDir(), "state.db")
	e.failStart = "api"
	err := withQuiescedWriters(context.Background(), e, stateFile, m, io.Discard, func(context.Context) error { return nil })
	if err == nil {
		t.Fatal("lost restart error")
	}
	if _, err := os.Stat(quiesceFile(stateFile, m.App)); err != nil {
		t.Fatal("lost recovery record")
	}
	e.failStart = ""
	if err := resumeWriters(context.Background(), e, quiesceFile(stateFile, m.App)); err != nil {
		t.Fatal(err)
	}
	if !e.containers[1].Running || e.containers[2].Running {
		t.Fatal("wrong writers restarted")
	}
}
func TestAppDataLockDrainsJobsAndHonorsCancellation(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.db")
	release, err := lockAppData(context.Background(), stateFile, "test", false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if unlock, err := lockAppData(ctx, stateFile, "test", true); err == nil {
		unlock()
		t.Fatal("backup overlapped a job")
	}
	release()
	unlock, err := lockAppData(context.Background(), stateFile, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestDataCommandsRefuseUnrecoveredPause(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.db")
	release, err := lockAppData(context.Background(), stateFile, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := savePausedWriters(quiesceFile(stateFile, "test"), nil); err != nil {
		t.Fatal(err)
	}
	release()
	if unlock, err := AcquireDataAccess(context.Background(), stateFile, "test"); err == nil {
		unlock()
		t.Fatal("command entered an unfinished pause")
	}
}
