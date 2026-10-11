package edge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylebegeman/bedrock/internal/docker"
	"github.com/kylebegeman/bedrock/internal/docker/dockertest"
)

func TestEdgeEnsureRefusesForeignContainersAndInspectionOutagesBeforeMutation(t *testing.T) {
	for _, outage := range []bool{false, true} {
		t.Run(map[bool]string{false: "foreign container", true: "inspection outage"}[outage], func(t *testing.T) {
			fake := dockertest.New(t)
			fake.Add(dockertest.Container{Name: Container, Running: true})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if outage && strings.HasSuffix(r.URL.Path, "/containers/"+Container+"/json") {
					http.Error(w, `{"message":"inspection unavailable"}`, http.StatusServiceUnavailable)
					return
				}
				fake.ServeHTTP(w, r)
			}))
			defer server.Close()
			t.Setenv("DOCKER_HOST", "tcp"+strings.TrimPrefix(server.URL, "http"))
			e, err := docker.Connect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			if err := Ensure(context.Background(), e, nil, io.Discard); err == nil {
				t.Fatal("claimed the edge despite an ownership or inspection failure")
			}
			for _, call := range fake.Calls() {
				if strings.HasPrefix(call, "POST ") || strings.HasPrefix(call, "DELETE ") {
					t.Fatalf("mutated Docker before edge preflight: %v", fake.Calls())
				}
			}
		})
	}
}
