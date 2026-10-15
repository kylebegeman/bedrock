package restic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kylebegeman/helios/internal/docker/dockertest"
)

func resticFaultServer(t *testing.T, fault func(http.ResponseWriter, *http.Request) bool) *dockertest.Server {
	t.Helper()
	fake := dockertest.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !fault(w, r) {
			fake.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("DOCKER_HOST", "tcp"+strings.TrimPrefix(server.URL, "http"))
	return fake
}

func TestResticRemovesItsHelperAfterAStartFailure(t *testing.T) {
	fake := resticFaultServer(t, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/start") {
			http.Error(w, `{"message":"cannot start"}`, http.StatusInternalServerError)
			return true
		}
		return false
	})
	var name string
	fake.OnCreate = func(c dockertest.Container) { name = c.Name }
	if _, err := runner(t).run(context.Background(), nil, "snapshots"); err == nil {
		t.Fatal("start failure was lost")
	}
	if _, exists := fake.Container(name); exists {
		t.Fatal("failed helper leaked")
	}
}

func TestResticReportsRemovalFailureEvenForAnExistingRepository(t *testing.T) {
	fake := resticFaultServer(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodDelete {
			http.Error(w, `{"message":"removal unavailable"}`, http.StatusInternalServerError)
			return true
		}
		return false
	})
	fake.OnCreate = func(c dockertest.Container) {
		fake.Update(c.Name, func(c *dockertest.Container) {
			c.ExitAfter = 1
			c.ExitCode = 1
			c.Logs = "repository already initialized"
		})
	}
	if _, err := runner(t).Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "removal unavailable") {
		t.Fatalf("lost helper cleanup failure: %v", err)
	}
}

func TestResticRejectsMalformedShellAssignments(t *testing.T) {
	for _, pair := range []string{"NO_VALUE", "BAD-NAME=value", "A;echo injected=value", "1NAME=value", "=value", "NAME=has\x00nul"} {
		if dir, err := secretsFile([]string{pair}); err == nil {
			os.RemoveAll(dir)
			t.Errorf("accepted malformed assignment %q", pair)
		}
	}
}
