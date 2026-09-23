package watch

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/bedrock/internal/docker/dockertest"
	"github.com/kylebegeman/bedrock/internal/state"
)

// appCondition is what the prober says about hello this round, or "".
func appCondition(t *testing.T, p *Prober) string {
	t.Helper()
	for _, c := range p.Observe(context.Background()) {
		if c.Key == "app:hello" {
			return c.Severity + ": " + c.Message
		}
	}
	return ""
}

func TestTheProberSeesAWorkloadStopRestartAndGo(t *testing.T) {
	fake := dockertest.New(t)
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	manifest := `{"app":"hello","workloads":{"work":{"kind":"worker","image":"x"}}}`
	rev := state.Revision{App: "hello", ID: "r1", Status: state.RevisionActive, Manifest: json.RawMessage(manifest),
		Containers: map[string]string{"work": "bedrock-hello-work-r1"}, CreatedAt: time.Now().UTC()}
	if err := store.SaveRevision(context.Background(), rev); err != nil {
		t.Fatal(err)
	}
	fake.Add(dockertest.Container{Name: "bedrock-edge", Running: true})
	fake.Add(dockertest.Container{Name: "bedrock-hello-work-r1", Running: true})
	p := NewProber(store)
	p.Root = t.TempDir()

	if got := appCondition(t, p); got != "" {
		t.Fatalf("a healthy app has a condition: %q", got)
	}
	fake.Update("bedrock-hello-work-r1", func(c *dockertest.Container) { c.RestartCount = 2 })
	if got := appCondition(t, p); !strings.Contains(got, "work keeps restarting (2 restarts)") {
		t.Fatalf("restarts: %q", got)
	}
	fake.Update("bedrock-hello-work-r1", func(c *dockertest.Container) { c.Running, c.ExitCode = false, 3 })
	if got := appCondition(t, p); !strings.HasPrefix(got, state.SeverityCritical) || !strings.Contains(got, "work isn't running") || !strings.Contains(got, "exit code 3") {
		t.Fatalf("stopped: %q", got)
	}
	fake.Remove("bedrock-hello-work-r1")
	if got := appCondition(t, p); !strings.Contains(got, "work has no container") {
		t.Fatalf("gone: %q", got)
	}
}
