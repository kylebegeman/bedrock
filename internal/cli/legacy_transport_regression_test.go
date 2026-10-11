package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyDeployRejectsSSHOptionDestination(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "ssh")
	marker := filepath.Join(dir, "called")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\ntouch \"$BEDROCK_TEST_SSH_MARKER\"\ncat >/dev/null\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEDROCK_SSH", wrapper)
	t.Setenv("BEDROCK_TEST_SSH_MARKER", marker)
	app := &app{stdout: io.Discard, stderr: io.Discard}
	if err := deployTo(context.Background(), app, t.TempDir(), "notes", "-oProxyCommand=unsafe"); err == nil {
		t.Fatal("SSH option accepted as destination")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("invalid destination executed wrapper: %v", err)
	}
}

func TestEverySourceTransportOmitsEnvironmentFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{".env", ".env.production", "nested/.env", "nested/.env.local", "public.txt"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("private"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := sourceFiles(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := writeTree(&archive, dir, files); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&archive)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(filepath.Base(header.Name), ".env") {
			t.Fatalf("environment file included in source: %s", header.Name)
		}
	}
}
