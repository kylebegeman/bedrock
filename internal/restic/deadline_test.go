package restic

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/helios/internal/docker/dockertest"
)

// A helper that never exits stands in for a repository that stops
// answering: restic waits on it until something stops the container.
func hangingRestic(t *testing.T) (*dockertest.Server, *[][]string) {
	t.Helper()
	fake := dockertest.New(t)
	var commands [][]string
	fake.OnCreate = func(c dockertest.Container) {
		commands = append(commands, append([]string(nil), c.Cmd...))
		fake.Update(c.Name, func(c *dockertest.Container) { c.Logs = `{"message_type":"status"}` })
	}
	return fake, &commands
}

func TestRepositoryCallsAreStoppedAtTheirDeadline(t *testing.T) {
	const limit = 50 * time.Millisecond
	for _, tc := range []struct {
		action string
		call   func(Runner) error
	}{
		{"init", func(r Runner) error { _, err := r.Ensure(context.Background()); return err }},
		{"unlock", func(r Runner) error { return r.Unlock(context.Background()) }},
		{"backup", func(r Runner) error {
			_, err := r.Backup(context.Background(), "site", nil, DataRoot)
			return err
		}},
		{"forget", func(r Runner) error { _, err := r.Forget(context.Background(), "site", Keep{Daily: 7}); return err }},
	} {
		t.Run(tc.action, func(t *testing.T) {
			fake, commands := hangingRestic(t)
			r := runner(t)
			r.Deadlines = Deadlines{Ensure: limit, Unlock: limit, Backup: limit, Forget: limit}
			started := time.Now()
			err := tc.call(r)
			// The wait polls once a second; anything near ForgetTimeout means
			// the deadline was not applied.
			if took := time.Since(started); took > 10*time.Second {
				t.Fatalf("the call outlived its deadline: %s", took)
			}
			var deadline *DeadlineError
			if !errors.As(err, &deadline) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("a stopped call must say it ran out of time: %v", err)
			}
			if deadline.Action != tc.action || deadline.Limit != limit || !strings.Contains(err.Error(), "restic "+tc.action+" did not finish within 50ms") {
				t.Fatalf("unclear deadline error: %v", err)
			}
			if len(*commands) != 1 || (*commands)[0][0] != tc.action {
				t.Fatalf("unexpected restic runs: %v", *commands)
			}
			// The helper is stopped, so restic gets to release its lock, and removed.
			if fake.Last(func(c string) bool { return strings.HasSuffix(c, "/stop") }) < 0 {
				t.Fatalf("the helper was not stopped: %v", fake.Calls())
			}
			if fake.Last(func(c string) bool { return strings.HasPrefix(c, "DELETE ") }) < 0 {
				t.Fatalf("the helper was not removed: %v", fake.Calls())
			}
		})
	}
}

func TestTheCallersCancellationIsNotReportedAsADeadline(t *testing.T) {
	fake := dockertest.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The caller goes away while the helper is being started, as a daemon
	// shutdown would cancel it.
	fake.OnCreate = func(dockertest.Container) { cancel() }
	r := runner(t)
	r.Deadlines.Forget = time.Hour
	_, err := r.Forget(ctx, "site", Keep{Daily: 7})
	var deadline *DeadlineError
	if !errors.Is(err, context.Canceled) || errors.As(err, &deadline) {
		t.Fatalf("cancellation must stay cancellation: %v", err)
	}
}

func TestUnlockOnlyRemovesStaleLocks(t *testing.T) {
	fake := dockertest.New(t)
	var command []string
	fake.OnCreate = func(c dockertest.Container) {
		command = c.Cmd
		fake.Update(c.Name, func(c *dockertest.Container) { c.ExitAfter = 1 })
	}
	if err := runner(t).Unlock(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(command, []string{"unlock"}) {
		t.Fatalf("unlock must never force removal of live locks: %v", command)
	}
}
