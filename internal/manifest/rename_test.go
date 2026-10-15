package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultManifestDiscoveryPreservesExistingApps(t *testing.T) {
	for _, name := range []string{"helios.yaml", "bedrock.yaml"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, name), []byte("app: hello\nworkloads:\n  web:\n    kind: worker\n    image: alpine:3.21\n"), 0600); err != nil {
				t.Fatal(err)
			}
			m, err := Load(dir)
			if err != nil || m.App != "hello" {
				t.Fatalf("existing %s not loaded: %v %+v", name, err, m)
			}
			report := CheckSource(dir, "")
			if !report.Valid || report.Manifest != name {
				t.Fatalf("check must report the selected file: %+v", report)
			}
		})
	}
}

func TestTwoDefaultManifestsRequireExplicitSelection(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"helios.yaml", "bedrock.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("app: hello\nworkloads:\n  web:\n    kind: worker\n    image: alpine:3.21\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "--manifest") {
		t.Fatalf("ambiguous defaults must refuse: %v", err)
	}
	for _, name := range []string{"helios.yaml", "bedrock.yaml"} {
		if _, err := LoadFile(dir, name); err != nil {
			t.Fatalf("explicit %s must work: %v", name, err)
		}
	}
	if r := CheckSource(dir, ""); r.Valid {
		t.Fatal("check guessed between two manifests")
	}
}

func TestManifestDiscoveryNeverBypassesAnUnsafeOrInvalidDefault(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "helios.yaml")
			if err := os.WriteFile(filepath.Join(dir, "bedrock.yaml"), []byte("app: hello\nworkloads:\n  web:\n    kind: worker\n    image: alpine:3.21\n"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink("missing", path)
			case "directory":
				err = os.Mkdir(path, 0700)
			case "invalid":
				err = os.WriteFile(path, []byte("bad: ["), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Load(dir); err == nil {
				t.Fatal("discovery ignored the unsafe or invalid new default")
			}
			if _, err := LoadFile(dir, "helios.yaml"); err == nil {
				t.Fatal("explicit unsafe or invalid file was accepted")
			}
		})
	}
}

func TestBothProductNamesRemainReserved(t *testing.T) {
	for _, name := range []string{"helios", "bedrock"} {
		if _, err := (Scaffold{App: name, Kind: Worker}).Render(); err == nil {
			t.Fatalf("reserved app %s was allowed", name)
		}
	}
}
