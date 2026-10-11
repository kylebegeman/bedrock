package remote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRejectsOversizedTargetConfigEvenAfterCompleteJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "targets.json")
	data := `{"version":1,"targets":{}}` + strings.Repeat(" ", 1<<20)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("oversized configuration accepted after bounded reader truncated trailing whitespace")
	}
}
