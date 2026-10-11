package cli

import (
	"context"
	"os"
	"testing"

	"github.com/kylebegeman/bedrock/internal/remote"
)

func TestUploadArtifactBoundsRemotePathResponse(t *testing.T) {
	config, _ := targetFixture(t)
	targets, err := remote.Load(config)
	if err != nil {
		t.Fatal(err)
	}
	target := targets.Targets["one"]
	if err := os.WriteFile(target.Binary, []byte("#!/bin/sh\nprintf '/%02000000d' 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "artifact-")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	app := &app{stateDir: t.TempDir(), stdout: os.Stdout, stderr: os.Stderr}
	if _, err := app.uploadArtifact(context.Background(), target, file, "fixture", "data"); err == nil {
		t.Fatal("unbounded remote artifact path accepted")
	}
}
