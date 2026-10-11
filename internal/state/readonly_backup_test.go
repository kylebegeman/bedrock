package state

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadOnlySnapshotCanProduceAnIsolatedMigrationCopy(t *testing.T) {
	s := open(t)
	readonly, err := OpenReadOnly(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer readonly.Close()
	if err := readonly.BackupTo(context.Background(), readonly.Path()); err == nil {
		t.Fatal("snapshot backup deleted its source")
	}
	if err := readonly.BackupTo(context.Background(), filepath.Join(t.TempDir(), "copy.db")); err != nil {
		t.Fatal(err)
	}
}

func TestStateAndCopiesKeepPrivateFileModes(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	copy := filepath.Join(dir, "copy.db")
	if err := s.BackupTo(context.Background(), copy); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{s.Path(), copy} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %o; want 600", path, info.Mode().Perm())
		}
	}
}

func TestFailedStateCopyPreservesExistingDestination(t *testing.T) {
	s := open(t)
	copy := filepath.Join(t.TempDir(), "keep.db")
	if err := os.WriteFile(copy, []byte("keep existing backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.BackupTo(ctx, copy); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled backup: %v", err)
	}
	got, err := os.ReadFile(copy)
	if err != nil || string(got) != "keep existing backup" {
		t.Fatalf("failed backup destroyed destination: %q %v", got, err)
	}
}

func TestOpenTightensExistingStateSidecars(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	original, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	paths := []string{path, path + "-wal", path + "-shm"}
	for _, file := range paths {
		if err := os.Chmod(file, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, file := range paths {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %o; want 600", file, info.Mode().Perm())
		}
	}
}
