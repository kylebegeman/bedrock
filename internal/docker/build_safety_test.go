package docker

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildRefusesDockerfileOutsideContextBeforeStartingCommands(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, path := range []string{"../outside.Dockerfile", "/tmp/outside.Dockerfile"} {
		err := Build(context.Background(), BuildSpec{Context: t.TempDir(), Dockerfile: path}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "Dockerfile must be relative to its build context") {
			t.Fatalf("path %q reached command execution: %v", path, err)
		}
	}
}

func TestBuildRefusesExternalDockerfileSymlink(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	external := filepath.Join(t.TempDir(), "Dockerfile")
	if err := os.WriteFile(external, []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(dir, "Dockerfile")); err != nil {
		t.Fatal(err)
	}
	err := Build(context.Background(), BuildSpec{Context: dir}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "Dockerfile inside build context") {
		t.Fatalf("external Dockerfile reached command execution: %v", err)
	}
}
