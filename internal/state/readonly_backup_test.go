package state

import (
	"context"
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
