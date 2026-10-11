package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func receiveArtifact(t *testing.T, stateDir, kind string, body []byte) (string, string, int) {
	t.Helper()
	input, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if _, err := input.Write(body); err != nil {
		t.Fatal(err)
	}
	input.Seek(0, 0)
	old := os.Stdin
	os.Stdin = input
	defer func() { os.Stdin = old }()
	sum := sha256.Sum256(body)
	var out, stderr bytes.Buffer
	code := Main([]string{"--state-dir", stateDir, "_remote-artifact", "--kind", kind, "--sha256", hex.EncodeToString(sum[:])}, &out, &stderr)
	return strings.TrimSpace(out.String()), stderr.String(), code
}

func TestCachedSourceIsVerifiedBeforeItIsReused(t *testing.T) {
	source := targetSource(t)
	archive, _, err := snapshotSource(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	defer os.Remove(archive.Name())
	body, err := os.ReadFile(archive.Name())
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	path, _, code := receiveArtifact(t, state, "source", body)
	if code != 0 {
		t.Fatal("initial source upload failed")
	}
	same, _, code := receiveArtifact(t, state, "source", body)
	if code != 0 || same != path {
		t.Fatal("unchanged source was not reusable")
	}
	if err := os.WriteFile(filepath.Join(path, "Dockerfile"), []byte("changed cached source"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := receiveArtifact(t, state, "source", body)
	if code == 0 || !strings.Contains(stderr, "cached source changed") {
		t.Fatal("corrupt cache was reused")
	}
	got, _ := os.ReadFile(filepath.Join(path, "Dockerfile"))
	if string(got) != "changed cached source" {
		t.Fatal("upload overwrote a possibly in-use cached tree")
	}
}

func TestCachedRestoreFileIsVerifiedBeforeItIsReused(t *testing.T) {
	state := t.TempDir()
	body := []byte("fixture data archive")
	path, _, code := receiveArtifact(t, state, "data", body)
	if code != 0 {
		t.Fatal("initial data upload failed")
	}
	if err := os.WriteFile(path, []byte("changed cached data"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := receiveArtifact(t, state, "data", body)
	if code == 0 || !strings.Contains(stderr, "cached artifact checksum changed") {
		t.Fatal("corrupt restore file was reused")
	}
}
