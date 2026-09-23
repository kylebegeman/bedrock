package cli

import (
	"testing"

	"github.com/kylebegeman/bedrock/internal/manifest"
	"os"
	"path/filepath"
	"strings"
)

// The name is what the app is called forever after, so it has to come out
// of ordinary repository URLs correctly and always be one a manifest takes.
func TestTheAppNameComesOutOfTheRepositoryURL(t *testing.T) {
	for repo, want := range map[string]string{
		"https://github.com/kylebegeman/dragon-writer":     "dragon-writer",
		"https://github.com/kylebegeman/dragon-writer.git": "dragon-writer",
		"https://github.com/kylebegeman/dragon-writer/":    "dragon-writer",
		"git@github.com:kylebegeman/begamin.git":           "begamin",
		"ssh://git@example.com:2222/team/My_Site.git":      "my-site",
		"file:///srv/repos/notes.git":                      "notes",
		"https://github.com/you/KyleBegeman.com":           "kylebegeman-com",
	} {
		got := appNameFromRepo(repo)
		if got != want {
			t.Fatalf("%s: got %q, want %q", repo, got, want)
		}
		// Whatever comes out has to be usable as an app name.
		if err := (manifest.Scaffold{App: got, Kind: manifest.Worker}).Check(); err != nil {
			t.Fatalf("%s produced %q, which a manifest refuses: %v", repo, got, err)
		}
	}
}

func TestAPlannedLaunchLeavesNoTreeBehind(t *testing.T) {
	stateDir := t.TempDir()
	repo := bareRepo(t, map[string]string{"Dockerfile": "FROM alpine:3.21\n"})
	out, errOut, code := run(t, stateDir, "launch", repo, "--host", "app.example.com", "--plan")
	if code != 0 || !strings.Contains(out, "plan digest") {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
	if !strings.Contains(errOut, "serve /healthz") {
		t.Fatalf("a launched web app should be told about its health path: %q", errOut)
	}
	if left := entries(t, filepath.Join(stateDir, "builds", "up")); len(left) != 0 {
		t.Fatalf("a planned launch left a tree behind: %v", left)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "launches")); err == nil {
		t.Fatal("launches go under builds now")
	}
}

func TestAFailedFetchLeavesNoTreeBehind(t *testing.T) {
	stateDir := t.TempDir()
	repo := bareRepo(t, map[string]string{"Dockerfile": "FROM alpine:3.21\n"})
	_, errOut, code := run(t, stateDir, "launch", repo, "--branch", "nope", "--host", "app.example.com")
	if code != 1 || !strings.Contains(errOut, "git clone") {
		t.Fatalf("code %d, err %q", code, errOut)
	}
	if left := entries(t, filepath.Join(stateDir, "builds", "up")); len(left) != 0 {
		t.Fatalf("a failed launch left a tree behind: %v", left)
	}
	_, errOut, code = run(t, stateDir, "launch", repo, "--branch", "-x", "--host", "app.example.com")
	if code != 1 || !strings.Contains(errOut, "isn't a branch name") {
		t.Fatalf("a branch that is a flag: code %d, err %q", code, errOut)
	}
}
