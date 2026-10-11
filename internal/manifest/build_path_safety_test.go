package manifest

import (
	"fmt"
	"strings"
	"testing"
)

func TestDockerfileMustStayInItsBuildContext(t *testing.T) {
	for _, path := range []string{"../../outside.Dockerfile", "../Dockerfile", "/tmp/Dockerfile"} {
		t.Run(path, func(t *testing.T) {
			_, err := Parse([]byte(fmt.Sprintf("app: site\nworkloads:\n  worker:\n    kind: worker\n    build: {context: server, dockerfile: %q}\n", path)))
			if err == nil || !strings.Contains(err.Error(), "build.dockerfile") {
				t.Fatalf("accepted escaping Dockerfile: %v", err)
			}
		})
	}
}
