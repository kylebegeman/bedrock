package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kylebegeman/bedrock/internal/secrets"
)

const siteYAML = "app: site\nworkloads:\n  web:\n    kind: web\n    image: alpine:3.21\n    port: 80\n    routes: [{host: site.example.com}]\n"

func TestAPlannedPreviewCopiesNoSecrets(t *testing.T) {
	stateDir := t.TempDir()
	repo := bareRepo(t, map[string]string{"bedrock.yaml": siteYAML})
	if _, errOut, code := withStdin(t, "verify_url=https://loom.example.com/api/cloud/auth/verify\n", stateDir, "integration", "set", "loom"); code != 0 {
		t.Fatalf("loom: %d %q", code, errOut)
	}
	if _, err := secrets.DefaultStore(stateDir).Set("site", "API_KEY", "v"); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := run(t, stateDir, "preview", "up", repo, "--branch", "main", "--domain", "preview.example.com", "--plan")
	if code != 0 {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
	if !strings.Contains(out, "plan digest") || !strings.Contains(out, "copy site's secrets to site-pr-main") {
		t.Fatalf("the plan should name the copy:\n%s", out)
	}
	if current, _ := secrets.DefaultStore(stateDir).Current("site-pr-main"); current != 0 {
		t.Fatalf("a plan copied secrets: version %d", current)
	}
	if left := entries(t, filepath.Join(stateDir, "builds", "site-pr-main")); len(left) != 0 {
		t.Fatalf("a planned preview left a tree behind: %v", left)
	}
	for _, name := range entries(t, filepath.Join(stateDir, "builds")) {
		if strings.HasPrefix(name, ".preview-") {
			t.Fatalf("a fetch directory survived: %s", name)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, "previews")); err == nil {
		t.Fatal("previews go under builds now")
	}
}

func TestPreviewListIsJSONWhenAsked(t *testing.T) {
	out, errOut, code := run(t, t.TempDir(), "preview", "ls", "--json")
	if code != 0 || strings.TrimSpace(out) != "[]" {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
}
