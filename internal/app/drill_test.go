package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kylebegeman/bedrock/internal/docker"
	"github.com/kylebegeman/bedrock/internal/manifest"
)

// A backup of files the app can't write restores an app that answers and
// can't save, so the drill fails and says which files.
func TestADrillFailsWhenTheWorkloadCantWriteWhatWasRestored(t *testing.T) {
	stuck, open := t.TempDir(), t.TempDir()
	for _, dir := range []string{stuck, open} {
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	for dir, mode := range map[string]os.FileMode{stuck: 0o644, open: 0o666} {
		path := filepath.Join(dir, "app.sqlite")
		if err := os.WriteFile(path, []byte("x"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	mountpoints := map[string]string{"scratch-data": stuck, "scratch-uploads": open}
	mountpoint := func(v string) (string, error) {
		if dir, ok := mountpoints[v]; ok {
			return dir, nil
		}
		return "", errors.New("no such volume")
	}
	volumes := map[string]string{"data": "scratch-data", "uploads": "scratch-uploads", "gone": "scratch-gone"}
	w := manifest.Workload{Mounts: []manifest.Mount{{Volume: "data", Path: "/data"}, {Volume: "data", Path: "/also"}, {Volume: "uploads", Path: "/uploads"}}}
	someoneElse := docker.User{UID: os.Getuid() + 1, GID: os.Getgid() + 1}

	err := restoredWritable("web", w, someoneElse, volumes, mountpoint)
	if err == nil || !strings.Contains(err.Error(), "can't write 1 file or directory in volume data, such as app.sqlite") {
		t.Fatalf("the drill should fail on data: %v", err)
	}
	if strings.Count(err.Error(), "volume data") != 1 || strings.Contains(err.Error(), "volume uploads") {
		t.Fatalf("names each stuck volume once and only those: %v", err)
	}
	if err := restoredWritable("web", w, docker.User{UID: os.Getuid(), GID: os.Getgid()}, volumes, mountpoint); err != nil {
		t.Fatalf("the owner can write it all: %v", err)
	}
	if err := restoredWritable("web", w, docker.User{}, volumes, mountpoint); err != nil {
		t.Fatalf("root can write it all: %v", err)
	}
	lost := manifest.Workload{Mounts: []manifest.Mount{{Volume: "gone", Path: "/gone"}}}
	if err := restoredWritable("web", lost, someoneElse, volumes, mountpoint); err == nil {
		t.Fatal("a restored volume that can't be found is a failed drill, not a pass")
	}
}
