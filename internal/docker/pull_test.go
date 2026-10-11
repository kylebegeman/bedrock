package docker

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestImagePullReportsErrorsInsideTheProgressStream(t *testing.T) {
	e, _ := interceptedEngine(t, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/images/create") {
			io.WriteString(w, "{\"status\":\"downloading\"}\n{\"errorDetail\":{\"message\":\"manifest missing\"},\"error\":\"manifest missing\"}\n")
			return true
		}
		return false
	})
	if err := e.Pull(context.Background(), "site:missing", io.Discard); err == nil || !strings.Contains(err.Error(), "manifest missing") {
		t.Fatalf("pull reported success after stream failure: %v", err)
	}
}
