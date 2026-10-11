package manifest

import (
	"fmt"
	"strings"
	"testing"
)

func TestManifestRejectsUnboundedOrNonfiniteResources(t *testing.T) {
	for _, resource := range []string{"memory: 0g", "memory: 9223372036854775807g", "memory: 8589934592g", "cpus: .nan", "cpus: .inf"} {
		if _, err := Parse([]byte("app: site\nworkloads:\n  worker:\n    kind: worker\n    image: alpine\n    resources: {" + resource + "}\n")); err == nil {
			t.Errorf("accepted %s", resource)
		}
	}
}

func TestMemorySizesStayWithinPositiveSignedBytes(t *testing.T) {
	for text, want := range map[string]int64{"": 0, "1k": 1024, "512m": 512 << 20, "2g": 2 << 30, "8589934591g": 8589934591 << 30} {
		if got, err := ParseMemory(text); err != nil || got != want {
			t.Errorf("%s: %d, %v; want %d", text, got, err, want)
		}
	}
	for _, text := range []string{"0k", "-1m", "1", "8589934592g", strings.Repeat("9", 40) + "k"} {
		if _, err := ParseMemory(text); err == nil {
			t.Errorf("accepted %q", text)
		}
	}
}

func TestAllManifestEntrypointsRejectAdditionalYAMLDocuments(t *testing.T) {
	first := "app: site\nworkloads:\n  worker: {kind: worker, image: alpine}\n"
	for _, extra := range []string{"---\napp: other\n", "---\n", "---\n["} {
		t.Run(fmt.Sprintf("%q", extra), func(t *testing.T) {
			if _, err := Parse([]byte(first + extra)); err == nil {
				t.Fatal("silently ignored an additional document")
			}
		})
	}
}
