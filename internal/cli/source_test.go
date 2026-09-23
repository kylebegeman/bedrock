package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// bareRepo makes a bare repository holding one commit of files and returns
// its file:// URL, the shape launch and preview fetch from.
func bareRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	work, bare := t.TempDir(), filepath.Join(t.TempDir(), "up.git")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	git(work, "init", "-q", "--initial-branch=main")
	for name, text := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(work, "add", "-A")
	git(work, "commit", "-q", "-m", "first")
	if out, err := exec.Command("git", "init", "-q", "--bare", "--initial-branch=main", bare).CombinedOutput(); err != nil {
		t.Fatalf("%s", out)
	}
	git(work, "push", "-q", bare, "main")
	return "file://" + bare
}

// entries lists a directory's entries; a directory that isn't there has
// none.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Name())
	}
	return names
}
