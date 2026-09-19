package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

// Every lane fixture's manifest must load, so a proof never fails on a
// typo the unit tests could have caught.
func TestTheLaneFixturesAreValid(t *testing.T) {
	dirs, err := filepath.Glob("../../lane/fixtures/*")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, dir := range dirs {
		if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
			continue
		}
		seen++
		if _, err := Load(dir); err != nil {
			t.Errorf("%s: %v", filepath.Base(dir), err)
		}
	}
	if seen < 3 {
		t.Fatalf("found %d fixture manifests", seen)
	}
}
