package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kylebegeman/bedrock/internal/compat"
)

func TestCapabilitiesDoesNotCreateStateOrSecrets(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	out, stderr, code := run(t, dir, "capabilities", "--json")
	var info compat.Info
	if code != 0 || stderr != "" || json.Unmarshal([]byte(out), &info) != nil || info.MachineID == "" || info.APIVersion != 1 || info.StateVersion != 1 || info.PersistedStateVersion != nil {
		t.Fatalf("code %d out %s err %s", code, out, stderr)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("capabilities created state")
	}
}
