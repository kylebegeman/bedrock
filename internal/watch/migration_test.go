package watch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/bedrock/internal/docker/dockertest"
	"github.com/kylebegeman/bedrock/internal/state"
)

func TestIntentionalMigrationFenceSuppressesAppDowntimeButNotHostFailures(t *testing.T) {
	fake := dockertest.New(t)
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	rev := state.Revision{App: "hello", ID: "r1", Status: state.RevisionActive,
		Manifest:   json.RawMessage(`{"app":"hello","workloads":{"web":{"kind":"web","image":"x","port":80}},"data":{"volumes":{"files":{}}},"checks":[{"url":"http://invalid.invalid/"}]}`),
		Containers: map[string]string{"web": "bedrock-hello-web-r1"}, CreatedAt: time.Now().UTC()}
	if err := store.SaveRevision(context.Background(), rev); err != nil {
		t.Fatal(err)
	}
	fake.Add(dockertest.Container{Name: "bedrock-hello-web-r1", Running: false})
	p := NewProber(store)
	p.Root = t.TempDir()
	fence := filepath.Join(filepath.Dir(store.Path()), "locks", "hello.migration.json")
	if err := os.MkdirAll(filepath.Dir(fence), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"handoff":{"app":"hello","migration":"move-0123456789abcdef","phase":"final","from":"source","to":"target","revision":"r1","manifest_digest":"` + strings.Repeat("a", 64) + `"},"role":"source","phase":"fenced"}`
	if err := os.WriteFile(fence, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := appCondition(t, p); got != "" {
		t.Fatalf("intentional source downtime reported: %s", got)
	}
	edgeReported := false
	for _, condition := range p.Observe(context.Background()) {
		if condition.Key == "host:edge" {
			edgeReported = true
		}
	}
	if !edgeReported {
		t.Fatal("source fence hid the host edge failure")
	}
	// A damaged fence is a visible failure, never maintenance suppression.
	for _, invalid := range []string{"{", `{"handoff":{"app":"other"},"role":"source"}`, strings.Replace(body, `"phase":"fenced"`, `"phase":"unknown"`, 1)} {
		if err := os.WriteFile(fence, []byte(invalid), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := appCondition(t, p); !strings.HasPrefix(got, state.SeverityCritical) || !strings.Contains(got, "migration fence") {
			t.Fatalf("corrupt migration fence was hidden: %s", got)
		}
	}
}
