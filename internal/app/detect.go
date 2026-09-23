package app

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kylebegeman/bedrock/internal/manifest"
)

// Detected is what a source tree looks like it wants to be.
type Detected struct {
	// Kind is the workload shape to scaffold.
	Kind manifest.Kind
	// Dir is the directory a static site is served from, relative to the
	// source root. It is empty for anything else.
	Dir string
	// Why is the file or directory that decided it, so the answer can be
	// argued with rather than trusted.
	Why string
}

// staticDirs are the places a built site is conventionally left, in the
// order they are believed. A repository with more than one of these is
// ambiguous only in theory: the first is the answer, and the manifest it
// writes can be edited.
var staticDirs = []string{"public", "dist", "build", "site", "_site"}

// Detect says what a source tree should be deployed as.
//
// It answers only where the answer is unambiguous from one file, because a
// wrong guess here becomes a manifest somebody then has to debug. A
// Dockerfile means the repository has already said how it builds and runs,
// so that is believed before anything else. Otherwise a directory of built
// files, or an index.html at the root, means a static site. Anything else
// is not guessed at.
func Detect(dir string) (*Detected, error) {
	if regular(filepath.Join(dir, manifest.FileName)) {
		return nil, fmt.Errorf("this source already has a %s; deploy it rather than starting it", manifest.FileName)
	}
	if regular(filepath.Join(dir, "Dockerfile")) {
		return &Detected{Kind: manifest.Web, Why: "Dockerfile"}, nil
	}
	for _, name := range staticDirs {
		if directory(filepath.Join(dir, name)) && hasIndex(filepath.Join(dir, name)) {
			return &Detected{Kind: manifest.Static, Dir: name, Why: name + "/index.html"}, nil
		}
	}
	if regular(filepath.Join(dir, "index.html")) {
		return &Detected{Kind: manifest.Static, Dir: ".", Why: "index.html"}, nil
	}
	return nil, fmt.Errorf("this source says nothing about how to run it: add a Dockerfile, or a %s of your own with bedrock init",
		manifest.FileName)
}

func regular(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

func directory(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir()
}

func hasIndex(dir string) bool { return regular(filepath.Join(dir, "index.html")) }
