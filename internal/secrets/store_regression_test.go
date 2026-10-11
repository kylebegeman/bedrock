package secrets

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStoreRejectsInvalidAppBeforeFilesystemAccess(t *testing.T) {
	store := DefaultStore(t.TempDir())
	for _, app := range []string{"", "..", "../escaped", "/tmp/escaped", "a/b"} {
		t.Run(app, func(t *testing.T) {
			if _, err := store.Current(app); err == nil {
				t.Fatal("Current accepted invalid app")
			}
			if _, err := store.Load(app, 0); err == nil {
				t.Fatal("Load accepted invalid app")
			}
			if _, err := store.Versions(app); err == nil {
				t.Fatal("Versions accepted invalid app")
			}
			if _, err := store.Set(app, "TOKEN", "v"); err == nil {
				t.Fatal("Set accepted invalid app")
			}
		})
	}
	if _, err := os.Stat(store.Dir); !os.IsNotExist(err) {
		t.Fatalf("invalid mutation touched filesystem: %v", err)
	}
}

func TestNamesFollowCommittedCurrentVersion(t *testing.T) {
	store := DefaultStore(t.TempDir())
	if _, _, _, err := store.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Set("notes", "COMMITTED", "v"); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(VersionInfo{Version: 2, Names: []string{"UNCOMMITTED"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir, "notes", "v2.meta"), data, 0600); err != nil {
		t.Fatal(err)
	}
	names, version, err := store.Names("notes")
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 || !reflect.DeepEqual(names, []string{"COMMITTED"}) {
		t.Fatalf("reported uncommitted metadata: %v version %d", names, version)
	}
}

func TestStoreRejectsInvalidCommittedVersionAndMismatchedCiphertext(t *testing.T) {
	store := DefaultStore(t.TempDir())
	if _, _, _, err := store.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Set("notes", "TOKEN", "v"); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(store.Dir, "notes", "current")
	for _, value := range []string{"0", "-1"} {
		if err := os.WriteFile(current, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Current("notes"); err == nil {
			t.Fatalf("invalid committed version %s accepted", value)
		}
	}
	data, err := os.ReadFile(filepath.Join(store.Dir, "notes", "v1.age"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir, "notes", "v2.age"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("notes", 2); err == nil {
		t.Fatal("ciphertext from another version accepted")
	}
}
