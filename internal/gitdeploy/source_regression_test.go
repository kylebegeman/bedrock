package gitdeploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewBuildDirAllocatesDistinctTreesForConcurrentDeploys(t *testing.T) {
	root := t.TempDir()
	start := make(chan struct{})
	paths := make(chan string, 100)
	var workers sync.WaitGroup
	for range 100 {
		workers.Go(func() {
			<-start
			path, err := NewBuildDir(root, "notes", "upload")
			if err != nil {
				t.Error(err)
				return
			}
			paths <- path
		})
	}
	close(start)
	workers.Wait()
	close(paths)
	seen := map[string]bool{}
	for path := range paths {
		if seen[path] {
			t.Fatalf("two deploys shared source tree %s", path)
		}
		seen[path] = true
	}
}

func TestExportCommitClosesProducerWhenExtractionFails(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "git")
	invalid := filepath.Join(dir, "invalid.tar")
	if err := os.WriteFile(invalid, append([]byte("invalid archive header"), make([]byte, 512)...), 0600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n/bin/cat \"$BEDROCK_TEST_INVALID_ARCHIVE\"\n/bin/dd if=/dev/zero bs=4096 count=1024 2>/dev/null\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BEDROCK_TEST_INVALID_ARCHIVE", invalid)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := ExportCommit(ctx, dir, "HEAD", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "source archive") {
		t.Fatalf("extraction failure waited for blocked producer or lost root error: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("export did not return until its producer context expired")
	}
}
