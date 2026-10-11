package watch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/bedrock/internal/docker"
	"github.com/kylebegeman/bedrock/internal/docker/dockertest"
	"github.com/kylebegeman/bedrock/internal/manifest"
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

func TestTheProberSeesAWorkloadThatCantWriteItsVolumeAndSeesItFixed(t *testing.T) {
	fake := dockertest.New(t)
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	manifest := `{"app":"hello","workloads":{"work":{"kind":"worker","image":"x","mounts":[{"volume":"data","path":"/data"}]}}}`
	rev := state.Revision{App: "hello", ID: "r1", Status: state.RevisionActive, Manifest: json.RawMessage(manifest),
		Containers: map[string]string{"work": "bedrock-hello-work-r1"}, CreatedAt: time.Now().UTC()}
	if err := store.SaveRevision(context.Background(), rev); err != nil {
		t.Fatal(err)
	}
	// The volume holds a database the test's own user owns; the workload
	// runs as someone else, the way a hand copy made by root leaves it.
	volume := t.TempDir()
	db := filepath.Join(volume, "hello.sqlite")
	if err := os.WriteFile(db, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(volume, 0o777); err != nil {
		t.Fatal(err)
	}
	user := fmt.Sprintf("%d:%d", os.Getuid()+1, os.Getgid()+1)
	fake.Add(dockertest.Container{Name: "bedrock-edge", Running: true})
	fake.Add(dockertest.Container{Name: "bedrock-hello-work-r1", Running: true, User: user,
		Mounts: []dockertest.Mount{{Type: "volume", Name: "bedrock-hello-data", Source: volume, Destination: "/data", RW: true}}})
	now := time.Now().UTC()
	p := NewProber(store)
	p.Root = t.TempDir()
	p.Now = func() time.Time { return now }

	got := appCondition(t, p)
	want := "work runs as " + user + " and can't write 1 file or directory in volume data, such as hello.sqlite"
	if !strings.HasPrefix(got, state.SeverityCritical) || !strings.Contains(got, want) || !strings.Contains(got, "bedrock doctor says how") {
		t.Fatalf("unwritable data: %q", got)
	}

	// Fixed by hand: the next rounds reuse the walk until it is due again.
	if err := os.Chmod(db, 0o666); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if got := appCondition(t, p); !strings.Contains(got, want) {
		t.Fatalf("a round inside the hour walks nothing: %q", got)
	}
	now = now.Add(DataCheckEvery)
	if got := appCondition(t, p); got != "" {
		t.Fatalf("fixed and walked again: %q", got)
	}

	// Root, and a user bedrock didn't set, need no walk.
	for _, u := range []string{"0:0", ""} {
		fake.Update("bedrock-hello-work-r1", func(c *dockertest.Container) { c.User = u })
		if err := os.Chmod(db, 0o644); err != nil {
			t.Fatal(err)
		}
		now = now.Add(DataCheckEvery)
		if got := appCondition(t, p); got != "" {
			t.Fatalf("user %q: %q", u, got)
		}
	}
}

func TestHealthProbeReachesIPv6Containers(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/health" {
			http.NotFound(w, request)
		}
	}))
	server.Listener = listener
	server.Start()
	defer server.Close()
	info := &docker.Info{IPs: map[string]string{docker.EdgeNetwork: "::1"}}
	workload := manifest.Workload{Kind: manifest.Web, Port: listener.Addr().(*net.TCPAddr).Port, Health: &manifest.Health{Path: "/health"}}
	if problem := healthProblem(context.Background(), info, workload); problem != "" {
		t.Fatalf("healthy IPv6 workload reported unavailable: %s", problem)
	}
}

func TestProberRetiresRestartHistoryForReplacedContainers(t *testing.T) {
	fake := dockertest.New(t)
	w, _ := newWatcher(t, nil)
	ctx := context.Background()
	body := json.RawMessage(`{"app":"hello","workloads":{"work":{"kind":"worker","image":"x"}}}`)
	fake.Add(dockertest.Container{Name: "bedrock-edge", Running: true})
	p := NewProber(w.Store)
	p.Root = t.TempDir()
	for _, id := range []string{"r1", "r2"} {
		container := "bedrock-hello-work-" + id
		fake.Add(dockertest.Container{Name: container, Running: true})
		if err := w.Store.SaveRevision(ctx, state.Revision{App: "hello", ID: id, Status: state.RevisionFailed, Manifest: body, Containers: map[string]string{"work": container}, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := w.Store.Activate(ctx, "hello", id, time.Now()); err != nil {
			t.Fatal(err)
		}
		p.Observe(ctx)
		if len(p.restarts) != 1 {
			t.Fatalf("old container restart history survived deployment: %v", p.restarts)
		}
	}
	if err := w.Store.SetRevisionStatus(ctx, "hello", "r2", state.RevisionFailed); err != nil {
		t.Fatal(err)
	}
	p.Observe(ctx)
	if len(p.restarts) != 0 {
		t.Fatalf("removed app restart history remains: %v", p.restarts)
	}
}

func TestDockerOutageCannotCertifyAppOrEdgeRecovery(t *testing.T) {
	ctx := context.Background()
	notifier := &fakeNotifier{}
	w, now := newWatcher(t, notifier)
	w.OpenAfter = 1
	body := json.RawMessage(`{"app":"hello","workloads":{"work":{"kind":"worker","image":"x"}}}`)
	if err := w.Store.SaveRevision(ctx, state.Revision{App: "hello", ID: "r1", Status: state.RevisionActive, Manifest: body, CreatedAt: *now}); err != nil {
		t.Fatal(err)
	}
	if err := w.Round(ctx, []Condition{down("hello"), {Key: "host:edge", Subject: "edge", Severity: state.SeverityCritical, Message: "edge is down"}}); err != nil {
		t.Fatal(err)
	}
	p := NewProber(w.Store)
	p.Root = t.TempDir()
	p.Now = func() time.Time { return *now }
	p.Connect = func(context.Context) (*docker.Engine, error) { return nil, errors.New("Docker unavailable") }
	for range 2 {
		*now = now.Add(time.Minute)
		if err := w.Round(ctx, p.Observe(ctx)); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"app:hello", "host:edge"} {
		if open, err := w.Store.OpenIncidentByKey(ctx, key); err != nil || open == nil {
			t.Fatalf("unobserved %s was reported recovered: %+v, %v", key, open, err)
		}
	}
}
