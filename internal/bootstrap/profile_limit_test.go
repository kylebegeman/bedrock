package bootstrap

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/kylebegeman/bedrock/internal/integration"
	"github.com/kylebegeman/bedrock/internal/remote"
	"github.com/kylebegeman/bedrock/internal/secrets"
)

func TestLoadRejectsOversizedProfileEvenAfterCompleteJSON(t *testing.T) {
	_, input, _ := bootstrapFixture(t)
	data, err := json.Marshal(input.Profile)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, strings.Repeat(" ", 1<<20)...)
	path := filepath.Join(t.TempDir(), "profile.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("oversized profile accepted after bounded reader truncated trailing whitespace")
	}
}

func TestProbeBoundsRemoteOSFacts(t *testing.T) {
	wrapper := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nprintf '%02000000d\\nx86_64\\nubuntu\\n24.04\\n0\\nsystemd\\n' 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Probe(context.Background(), remote.Transport{Program: []string{wrapper}}, remote.Target{SSH: "fixture"}); err == nil {
		t.Fatal("unbounded remote OS facts accepted")
	}
}

func TestBootstrapBoundsBufferedResponses(t *testing.T) {
	for _, tc := range []struct{ step, command string }{
		{"capabilities", "capabilities"},
		{"capabilities", "version"},
		{"identity", "secret"},
		{"integrations", "secret"},
	} {
		t.Run(tc.step+"/"+tc.command, func(t *testing.T) {
			def, in, _ := bootstrapFixture(t)
			identity, err := age.GenerateX25519Identity()
			if err != nil {
				t.Fatal(err)
			}
			if tc.step == "integrations" {
				def.Secrets = secrets.DefaultStore(t.TempDir())
				if _, _, _, err := def.Secrets.EnsureKey(); err != nil {
					t.Fatal(err)
				}
				if _, _, err := integration.Set(def.Secrets, "email", map[string]string{"smtp_host": "smtp.fixture.invalid", "smtp_port": "1025", "from": "fixture@example.com", "to": "owner@example.com"}); err != nil {
					t.Fatal(err)
				}
				in.Profile.Integrations = []string{"email"}
				in.SecretsVersion, err = def.Secrets.Current(integration.App)
				if err != nil {
					t.Fatal(err)
				}
				in.SecretsSHA256, err = def.Secrets.SealedVersionSHA256(integration.App, in.SecretsVersion)
				if err != nil {
					t.Fatal(err)
				}
			}
			wrapper := filepath.Join(t.TempDir(), "ssh")
			script := `#!/bin/sh
for last do :; done
case "$last" in
 *"'capabilities'"*) printf '%s' '{"api_version":1,"manifest_version":1,"machine_id":"linux:fixture"}' ;;
 *"'version'"*) printf '%s' '{"version":"fixture-dev","os":"linux","arch":"arm64"}' ;;
 *"'secret'"*) printf '%s' '` + identity.Recipient().String() + `' ;;
esac
case "$last" in
 *"'` + tc.command + `'"*) printf '%2000000s' '' ;;
esac
`
			if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			def.Transport.Program = []string{wrapper}
			raw, err := json.Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := def.Plan(context.Background(), raw)
			if err != nil {
				t.Fatal(err)
			}
			for _, step := range plan.Steps {
				if step.Name == tc.step {
					if err := step.Apply(context.Background(), io.Discard); err == nil {
						t.Fatalf("oversized %s response: %v", tc.command, err)
					}
					return
				}
			}
			t.Fatalf("missing %s step", tc.step)
		})
	}
}
