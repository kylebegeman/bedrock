package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/bedrock/internal/state"
)

// The edge trusts Cloudflare to name the visitor only when the machine
// keeps Cloudflare's list, and a list that is missing, broken or far wider
// than Cloudflare's is not trusted at all.
func TestTheEdgeTrustsOnlyTheListTheMachineKeeps(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	good := write("good.json", `{"ipv4_cidrs":["173.245.48.0/20"],"ipv6_cidrs":["2400:cb00::/32"]}`)
	if got := keptProxies(good); !slices.Equal(got, []string{"173.245.48.0/20", "2400:cb00::/32"}) {
		t.Fatalf("kept list: %v", got)
	}
	for name, body := range map[string]string{
		"broken.json": `{"ipv4_cidrs":`,
		"wide.json":   `{"ipv4_cidrs":["0.0.0.0/0"]}`,
		"empty.json":  `{}`,
	} {
		if got := keptProxies(write(name, body)); got != nil {
			t.Errorf("%s was trusted: %v", name, got)
		}
	}
	if got := keptProxies(filepath.Join(dir, "missing.json")); got != nil {
		t.Fatalf("a missing list was trusted: %v", got)
	}
}

func TestTheEdgeConfigTrustsCloudflareWhenTheMachineKeepsItsRanges(t *testing.T) {
	ctx := context.Background()
	m, _ := loadCore(t)
	raw, _ := json.Marshal(m)
	store := goldenStore(t)
	containers := map[string]string{}
	for _, name := range m.WorkloadNames() {
		containers[name] = "bedrock-loom-" + name + "-r1"
	}
	if err := store.SaveRevision(ctx, state.Revision{App: "loom", ID: "r1", Status: state.RevisionActive, Manifest: raw, Images: map[string]string{}, Containers: containers, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	sec := newSecrets(t)
	saved := cloudflareProxies
	t.Cleanup(func() { cloudflareProxies = saved })

	cloudflareProxies = func() []string { return nil }
	cfg, err := EdgeConfig(ctx, store, sec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), "trusted_proxies") || !strings.Contains(string(cfg), `"X-Forwarded-For": [`) {
		t.Fatalf("without a list nothing is trusted, and apps are still told who connected:\n%s", cfg)
	}
	cloudflareProxies = func() []string { return []string{"173.245.48.0/20"} }
	cfg, err = EdgeConfig(ctx, store, sec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), `"173.245.48.0/20"`) || !strings.Contains(string(cfg), `"trusted_proxies_strict": 1`) {
		t.Fatalf("the kept ranges are not trusted:\n%s", cfg)
	}
}
