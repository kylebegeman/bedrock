package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/helios/internal/manifest"
	"github.com/kylebegeman/helios/internal/state"

	"github.com/kylebegeman/helios/internal/secrets"
)

const siteYAML = "app: site\nworkloads:\n  web:\n    kind: web\n    image: alpine:3.21\n    port: 80\n    routes: [{host: site.example.com}]\n"

func TestAPlannedPreviewCopiesNoSecrets(t *testing.T) {
	stateDir := t.TempDir()
	repo := bareRepo(t, map[string]string{"helios.yaml": siteYAML})
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

func saveCLIPreview(t *testing.T, stateDir, name, parent, branch string, pinned bool, at time.Time) {
	t.Helper()
	store, err := state.Open(filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := manifest.Manifest{App: name, Preview: &manifest.Preview{Of: parent, Branch: branch, Pinned: pinned}, Workloads: map[string]manifest.Workload{"web": {Kind: manifest.Web, Image: "image", Port: 80, Routes: []manifest.Route{{Host: name + ".example.com", Auth: manifest.AuthLoom}}}}}
	raw, _ := json.Marshal(m)
	if err := store.SaveRevision(context.Background(), state.Revision{App: name, ID: "r1", Status: state.RevisionActive, Manifest: raw, CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewPinAndCleanupPlansUseMetadataWithoutDeletingAnything(t *testing.T) {
	stateDir := t.TempDir()
	saveCLIPreview(t, stateDir, "site-pr-nav", "site", "feature/nav", false, time.Now().Add(-14*24*time.Hour))
	out, errOut, code := run(t, stateDir, "preview", "pin", "site", "--branch", "feature/nav", "--plan")
	if code != 0 || !strings.Contains(out, "pin preview site-pr-nav") {
		t.Fatalf("pin plan: %d %s %s", code, out, errOut)
	}
	out, errOut, code = run(t, stateDir, "preview", "prune", "site", "--older-than", "7d", "--plan")
	if code != 0 || !strings.Contains(out, "remove preview site-pr-nav") || !strings.Contains(out, "including its data and secrets") {
		t.Fatalf("prune plan: %d %s %s", code, out, errOut)
	}
	out, errOut, code = run(t, stateDir, "preview", "pin", "site", "--branch", "feature/nav", "--yes")
	if code != 0 {
		t.Fatalf("pin: %d %s %s", code, out, errOut)
	}
	out, errOut, code = run(t, stateDir, "preview", "ls", "--json")
	var rows []struct {
		Pinned bool `json:"pinned"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || !rows[0].Pinned {
		t.Fatalf("pin metadata: %d %s %s", code, out, errOut)
	}
	out, errOut, code = run(t, stateDir, "preview", "prune", "--older-than", "7d", "--plan")
	if code != 0 || !strings.Contains(out, "no unpinned previews match") {
		t.Fatalf("pinned prune: %d %s %s", code, out, errOut)
	}
	out, errOut, code = run(t, stateDir, "preview", "down", "site", "--branch", "feature/nav", "--plan")
	if code != 0 || !strings.Contains(out, "remove preview site-pr-nav") {
		t.Fatalf("explicit pinned down: %d %s %s", code, out, errOut)
	}
	out, errOut, code = run(t, stateDir, "preview", "unpin", "site-pr-nav", "--yes")
	if code != 0 {
		t.Fatalf("unpin: %d %s %s", code, out, errOut)
	}
	out, errOut, code = run(t, stateDir, "preview", "ls", "--json")
	if code != 0 || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || rows[0].Pinned {
		t.Fatalf("unpin metadata: %d %s %s", code, out, errOut)
	}
}

func TestPreviewCleanupRefusesAnOrdinaryAppWithAPreviewLookingName(t *testing.T) {
	stateDir := t.TempDir()
	store, err := state.Open(filepath.Join(stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse([]byte(strings.ReplaceAll(siteYAML, "app: site", "app: site-pr-nav")))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(m)
	if err := store.SaveRevision(context.Background(), state.Revision{App: m.App, ID: "r1", Status: state.RevisionActive, Manifest: raw, CreatedAt: time.Now().Add(-14 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	_, errOut, code := run(t, stateDir, "preview", "down", "site-pr-nav", "--plan")
	if code != 1 || !strings.Contains(errOut, "not a running preview") {
		t.Fatalf("ordinary app down: %d %s", code, errOut)
	}
	out, errOut, code := run(t, stateDir, "preview", "prune", "--plan")
	if code != 0 || !strings.Contains(out, "no unpinned previews match") {
		t.Fatalf("ordinary app prune: %d %s %s", code, out, errOut)
	}
}

func TestPreviewRedeployPreservesPinAndReportsIndividualUnsupportedChecks(t *testing.T) {
	stateDir := t.TempDir()
	saveCLIPreview(t, stateDir, "site-pr-main", "site", "main", true, time.Now())
	repo := bareRepo(t, map[string]string{"helios.yaml": siteYAML + "checks:\n  - url: https://site.example.com/\n    contains: expected content\n  - url: https://external.example.com/\n    context: edge\n"})
	if _, errOut, code := withStdin(t, "verify_url=https://loom.example.com/api/cloud/auth/verify\n", stateDir, "integration", "set", "loom"); code != 0 {
		t.Fatalf("loom: %d %s", code, errOut)
	}
	out, errOut, code := run(t, stateDir, "preview", "up", repo, "--branch", "main", "--domain", "preview.example.com", "--plan")
	if code != 0 || !strings.Contains(errOut, "check https://external.example.com/ skipped:") || !strings.Contains(out, "run 1 check(s) against the new containers") || !strings.Contains(out, "1 sign-in guard") {
		t.Fatalf("preview checks: %d %s %s", code, out, errOut)
	}
	// Inspect the derived manifest directly, since --plan intentionally cleans
	// its source tree and must not persist changed preview metadata.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, manifest.FileName), []byte(siteYAML), 0600); err != nil {
		t.Fatal(err)
	}
	p := &previewUp{a: &app{stateDir: stateDir}, branch: "main", domain: "preview.example.com", manifestName: manifest.FileName}
	_, derived, err := p.derive(context.Background(), dir, "", false)
	if err != nil || !derived.Preview.Pinned {
		t.Fatalf("pin not preserved: %+v %v", derived, err)
	}
	p.pinSet = true
	_, derived, err = p.derive(context.Background(), dir, "", false)
	if err != nil || derived.Preview.Pinned {
		t.Fatalf("explicit --pin=false ignored: %+v %v", derived, err)
	}
}
