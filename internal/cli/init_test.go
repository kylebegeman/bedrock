package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kylebegeman/bedrock/internal/manifest"
)

func TestInitWritesAManifestThatLoads(t *testing.T) {
	dir := t.TempDir()
	out, errOut, code := run(t, t.TempDir(), "init", "acme", "--host", "acme.example.com", "--postgres", "--dir", dir)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	m, err := manifest.Load(dir)
	if err != nil {
		t.Fatalf("the written manifest doesn't load: %v", err)
	}
	if m.App != "acme" || !m.HasData() {
		t.Fatalf("got %+v", m)
	}
	for _, want := range []string{"Wrote ", "Still to do:", "bedrock deploy " + dir} {
		if !strings.Contains(out, want) {
			t.Fatalf("output never says %q: %q", want, out)
		}
	}
}

// Overwriting an app's manifest by accident would lose whatever was written
// into it by hand, so it takes a second, explicit ask.
func TestInitRefusesToOverwriteWithoutBeingTold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, manifest.FileName)
	if err := os.WriteFile(path, []byte("app: mine\nworkloads:\n  w:\n    kind: worker\n    image: alpine:3.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, errOut, code := run(t, t.TempDir(), "init", "acme", "--host", "acme.example.com", "--dir", dir)
	if code == 0 {
		t.Fatal("an existing manifest must not be overwritten")
	}
	if !strings.Contains(errOut, "--force") {
		t.Fatalf("the error should name the way through: %q", errOut)
	}
	if body, _ := os.ReadFile(path); !strings.Contains(string(body), "app: mine") {
		t.Fatal("the existing manifest was changed anyway")
	}

	if _, errOut, code = run(t, t.TempDir(), "init", "acme", "--host", "acme.example.com", "--dir", dir, "--force"); code != 0 {
		t.Fatalf("--force should write: %q", errOut)
	}
	if m, err := manifest.Load(dir); err != nil || m.App != "acme" {
		t.Fatalf("%v %+v", err, m)
	}
}

func TestInitReportsWhereItPutThingsAsJSON(t *testing.T) {
	dir := t.TempDir()
	out, _, code := run(t, t.TempDir(), "--json", "init", "acme", "--kind", "static", "--host", "acme.example.com", "--dir", dir)
	if code != 0 {
		t.Fatal(out)
	}
	var got struct {
		Path string   `json:"path"`
		Next []string `json:"next"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %q", err, out)
	}
	if got.Path != filepath.Join(dir, manifest.FileName) {
		t.Fatalf("path is %q", got.Path)
	}
	if len(got.Next) == 0 {
		t.Fatal("a static app still needs its files put somewhere")
	}
}

// A directory that isn't there is the likeliest mistake, and the raw
// "no such file or directory" from the write gave no hint what to do.
func TestInitSaysWhenTheDirectoryIsMissing(t *testing.T) {
	_, errOut, code := run(t, t.TempDir(), "init", "acme", "--host", "acme.example.com", "--dir", filepath.Join(t.TempDir(), "nope"))
	if code == 0 {
		t.Fatal("a missing directory must be refused")
	}
	if !strings.Contains(errOut, "doesn't exist") {
		t.Fatalf("unhelpful error: %q", errOut)
	}
}
