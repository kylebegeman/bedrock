package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kylebegeman/bedrock/internal/manifest"
)

func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A repository with a Dockerfile has already said how it builds and runs,
// so nothing else gets a vote.
func TestADockerfileWinsOverEverything(t *testing.T) {
	dir := tree(t, map[string]string{
		"Dockerfile":        "FROM alpine\n",
		"public/index.html": "<h1>hi</h1>",
		"index.html":        "<h1>hi</h1>",
	})
	got, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != manifest.Web || got.Dir != "" || got.Why != "Dockerfile" {
		t.Fatalf("%+v", got)
	}
}

func TestABuiltSiteIsStatic(t *testing.T) {
	for _, name := range staticDirs {
		dir := tree(t, map[string]string{name + "/index.html": "<h1>hi</h1>"})
		got, err := Detect(dir)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Kind != manifest.Static || got.Dir != name {
			t.Fatalf("%s: %+v", name, got)
		}
	}
}

func TestAnIndexAtTheRootIsStatic(t *testing.T) {
	got, err := Detect(tree(t, map[string]string{"index.html": "<h1>hi</h1>"}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != manifest.Static || got.Dir != "." {
		t.Fatalf("%+v", got)
	}
}

// A directory named public with no index in it is not a site, and calling
// it one produces a deploy that serves nothing.
func TestADirectoryWithoutAnIndexIsNotASite(t *testing.T) {
	_, err := Detect(tree(t, map[string]string{"public/notes.txt": "hello"}))
	if err == nil {
		t.Fatal("an empty-looking public directory must not be called a site")
	}
}

// Guessing wrong produces a manifest somebody has to debug, so a source
// that says nothing is refused and told what to do.
func TestASourceThatSaysNothingIsRefused(t *testing.T) {
	_, err := Detect(tree(t, map[string]string{"README.md": "# hello", "main.go": "package main"}))
	if err == nil {
		t.Fatal("an unrecognisable source must be refused")
	}
	for _, want := range []string{"Dockerfile", "bedrock init"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error should suggest %q: %v", want, err)
		}
	}
}

// Launching something that already has a manifest would overwrite a
// person's own work with a guess.
func TestASourceThatAlreadyHasAManifestIsRefused(t *testing.T) {
	dir := tree(t, map[string]string{
		manifest.FileName: "app: mine\n",
		"Dockerfile":      "FROM alpine\n",
	})
	_, err := Detect(dir)
	if err == nil || !strings.Contains(err.Error(), "deploy it rather than starting it") {
		t.Fatalf("want a refusal pointing at deploy, got %v", err)
	}
}

// What Detect returns has to produce a manifest that actually loads.
func TestWhatIsDetectedScaffoldsIntoAValidManifest(t *testing.T) {
	for name, dir := range map[string]string{
		"web":    tree(t, map[string]string{"Dockerfile": "FROM alpine\n"}),
		"static": tree(t, map[string]string{"public/index.html": "<h1>hi</h1>"}),
	} {
		got, err := Detect(dir)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		s := manifest.Scaffold{App: "acme", Kind: got.Kind, Host: "acme.example.com"}
		body, err := s.Render()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := manifest.Parse(body); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
