package cli

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/kylebegeman/bedrock/internal/host"
)

func TestDoctorJSONPreservesFailureExitStatus(t *testing.T) {
	dir := t.TempDir()
	// Missing executables and an owned absent socket keep all probes local and
	// guarantee a failed system check on every test platform.
	t.Setenv("PATH", dir)
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(dir, "missing-docker.sock"))
	t.Setenv("DOCKER_TLS_VERIFY", "")
	t.Setenv("DOCKER_CERT_PATH", "")
	output, stderr, code := run(t, dir, "doctor", "--json")
	var report struct {
		Worst host.Verdict `json:"worst"`
	}
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("invalid doctor JSON: %v", err)
	}
	if report.Worst != host.Fail {
		t.Fatalf("fixture did not fail: %s", output)
	}
	if code != 1 {
		t.Fatalf("failed doctor report exited %d: %s", code, stderr)
	}
}
