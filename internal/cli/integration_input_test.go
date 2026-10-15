package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kylebegeman/helios/internal/kernel"
	"github.com/kylebegeman/helios/internal/secrets"
)

func TestMalformedIntegrationInputNeverEchoesTheSuppliedValue(t *testing.T) {
	stateDir := t.TempDir()
	if _, _, _, err := secrets.DefaultStore(stateDir).EnsureKey(); err != nil {
		t.Fatal(err)
	}
	private := "fixture-pasted-private-credential"
	_, stderr, code := withStdin(t, "# ignored\n\n"+private+"\n", stateDir, "integration", "set", "email")
	if code == 0 || strings.Contains(stderr, private) || !strings.Contains(stderr, "line 3") {
		t.Fatalf("malformed input was not safely explained: code %d, %s", code, stderr)
	}
}

func TestRemoteCredentialPipesRemainUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		value string
	}{
		{"secret", []string{"secret", "set", "fixture", "TOKEN"}, "fixture-private-value\n"},
		{"integration", []string{"integration", "set", "email"}, "smtp_password=fixture-private-value\nsmtp_host=smtp.example.test\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, record, captured := targetArtifactFixture(t)
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			original := os.Stdin
			os.Stdin = read
			defer func() { os.Stdin = original; read.Close() }()
			go func() { _, _ = write.WriteString(tc.value); write.Close() }()
			out, stderr, code := targetRun(t, config, tc.args...)
			if code != 0 {
				t.Fatalf("%d %s", code, stderr)
			}
			raw, err := os.ReadFile(filepath.Join(captured, "applied-stdin"))
			if err != nil || string(raw) != tc.value {
				t.Fatalf("pipe changed: %q %v", raw, err)
			}
			argv, err := os.ReadFile(record)
			if err != nil || strings.Contains(string(argv)+out+stderr, "fixture-private-value") {
				t.Fatal("credential escaped stdin")
			}
		})
	}
}

func TestRemotePlanApprovalBindsStateAndSocketOverrides(t *testing.T) {
	for _, flag := range []string{"--state-dir", "--socket"} {
		t.Run(flag, func(t *testing.T) {
			config, record := targetFixture(t)
			args := []string{"kernel", "exercise", "--dir", "/tmp/markers"}
			planArgs := append(append([]string{}, args...), flag+"=/fixture/one", "--plan", "--json")
			out, stderr, code := targetRun(t, config, planArgs...)
			var plan kernel.PlanView
			if code != 0 || json.Unmarshal([]byte(out), &plan) != nil {
				t.Fatalf("%d %s %s", code, out, stderr)
			}
			applyArgs := append(append([]string{}, args...), flag+"=/fixture/two", "--digest", plan.Digest)
			_, stderr, code = targetRun(t, config, applyArgs...)
			if code == 0 || !strings.Contains(stderr, "read --plan again") {
				t.Fatalf("changed state context accepted: %d %s", code, stderr)
			}
			if _, err := os.Stat(record); !os.IsNotExist(err) {
				t.Fatal("changed state context applied")
			}
		})
	}
}
