package docker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStaticContextReadsAnInternalSymlinkToItsContentDirectory(t *testing.T) {
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "content"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "content", "index.html"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("content", filepath.Join(source, "public")); err != nil {
		t.Fatal(err)
	}
	context, err := StaticContext(source, "public")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(context)
	if got, err := os.ReadFile(filepath.Join(context, "site", "index.html")); err != nil || string(got) != "hello" {
		t.Fatalf("static content was dropped: %q %v", got, err)
	}
}
