package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

// Every lane fixture's manifest must load, so a proof never fails on a
// typo the unit tests could have caught.
func TestTheLaneFixturesAreValid(t *testing.T) {
	dirs, err := filepath.Glob("../../lane/fixtures/*")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
			continue
		}
		seen++
		if _, err := Load(dir); err != nil {
			t.Errorf("%s: %v", filepath.Base(dir), err)
		}
	}
	if seen < 3 {
		t.Fatalf("found %d fixture manifests", seen)
	}
}

func TestAnotherManifestIsAPlainYAMLFileAtTheSourcesRoot(t *testing.T) {
	dir := t.TempDir()
	body := []byte("app: runner\nworkloads:\n  run:\n    kind: worker\n    image: alpine:3.21\n")
	if err := os.WriteFile(filepath.Join(dir, "quark.runner.yaml"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadFile(dir, "quark.runner.yaml")
	if err != nil || m.App != "runner" {
		t.Fatalf("%v %+v", err, m)
	}
	for _, name := range []string{"", "../quark.yaml", "sub/quark.yaml", ".quark.yaml", "quark.json", "/etc/quark.yaml"} {
		if _, err := LoadFile(dir, name); err == nil {
			t.Fatalf("%q must be refused", name)
		}
	}
	outside := filepath.Join(t.TempDir(), "elsewhere.yaml")
	if err := os.WriteFile(outside, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(dir, "linked.yaml"); err == nil {
		t.Fatal("a manifest that links out of the source must be refused")
	}
}
