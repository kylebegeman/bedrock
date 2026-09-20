package cli

import (
	"strings"
	"testing"

	"github.com/kylebegeman/bedrock/internal/docker"
)

func workload(name, app, wl string, nets ...string) docker.Exposure {
	return docker.Exposure{
		Name: name, App: app, Running: true, User: "65534:65534", NoNewPrivileges: true, ReadOnlyRoot: true,
		Networks: nets, MemoryBytes: 2 << 30, PidsLimit: 4096,
		Labels: map[string]string{docker.LabelOwner: docker.OwnerValue, docker.LabelApp: app, docker.LabelWorkload: wl},
	}
}

func TestIsolatedAppsShareNothing(t *testing.T) {
	xs := []docker.Exposure{
		workload("bedrock-a-web-1", "a", "web", "bedrock-a", "bedrock.edge.a"),
		workload("bedrock-b-web-1", "b", "web", "bedrock-b", "bedrock.edge.b"),
		{Name: "bedrock-edge", Running: true, Networks: []string{"bedrock-edge", "bedrock.edge.a", "bedrock.edge.b"}, Ports: []string{"0.0.0.0:443/tcp"},
			Labels: map[string]string{docker.LabelOwner: docker.OwnerValue, docker.LabelApp: "edge"}},
	}
	r := buildExposure(xs)
	if len(r.Shared) != 0 || len(r.Findings) != 0 {
		t.Fatalf("shared %v findings %v", r.Shared, r.Findings)
	}
	if got := strings.Join(r.Containers[0].Nets, ","); got != "own,edge" {
		t.Fatalf("network roles: %s", got)
	}
}

func TestOldContainersAndOpenings(t *testing.T) {
	old := workload("bedrock-a-web-0", "a", "web", "bedrock-a", "bedrock-edge")
	old.AllCapabilities, old.ReadOnlyRoot = true, false
	b := workload("bedrock-b-web-1", "b", "web", "bedrock-b", "bedrock-edge")
	b.User = "0:0"
	b.Capabilities = []string{"NET_BIND_SERVICE"}
	b.Mounts = []string{"/var/run/docker.sock:/var/run/docker.sock"}
	runner := workload("bedrock-c-runner-1", "c", "runner", "bedrock-c")
	runner.Privileged = true
	stray := docker.Exposure{Name: "lane-minio", Running: true, Ports: []string{"127.0.0.1:9000/tcp"}, Labels: map[string]string{}}
	r := buildExposure([]docker.Exposure{old, b, runner, stray})
	if len(r.Shared) != 1 || r.Shared[0] != "bedrock-edge: a, b" {
		t.Fatalf("shared: %v", r.Shared)
	}
	joined := strings.Join(r.Findings, "\n")
	for _, want := range []string{
		"bedrock-a-web-0 runs with Docker's default privileges: started before isolation; redeploy a",
		"bedrock-a-web-0 can write its root filesystem",
		"bedrock-b-web-1 runs as root",
		"bedrock-b-web-1 keeps NET_BIND_SERVICE",
		"bedrock-b-web-1 mounts a path from the machine: /var/run/docker.sock",
		"bedrock-c-runner-1 is privileged",
		"lane-minio isn't managed by bedrock and publishes 127.0.0.1:9000/tcp",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("findings lack %q:\n%s", want, joined)
		}
	}
}
