package bootstrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/kylebegeman/bedrock/internal/host"
	"github.com/kylebegeman/bedrock/internal/integration"
	"github.com/kylebegeman/bedrock/internal/kernel"
	"github.com/kylebegeman/bedrock/internal/remote"
	"github.com/kylebegeman/bedrock/internal/secrets"
	"github.com/kylebegeman/bedrock/internal/state"
)

func bootstrapFixture(t *testing.T) (Definition, Input, string) {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "commands")
	source := filepath.Join(dir, "candidate")
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$BOOTSTRAP_TEST_COMMANDS"
case "$1" in
 capabilities) printf '%s\n' '{"api_version":1,"manifest_version":1,"machine_id":"linux:fixture"}' ;;
 version) printf '%s\n' '{"version":"fixture-dev","os":"linux","arch":"arm64"}' ;;
 secret) printf '%s\n' '` + identity.Recipient().String() + `' ;;
 doctor) if test "${BOOTSTRAP_TEST_FAIL:-}" = yes; then printf 'fixture doctor failed\n' >&2; exit 1; fi ;;
esac
`
	if err := os.WriteFile(source, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(script))
	wrapper := filepath.Join(dir, "ssh")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nfor last do :; done\nexec /bin/sh -c \"$last\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BOOTSTRAP_TEST_COMMANDS", record)
	in := Input{TargetName: "fixture", Target: remote.Target{SSH: "fixture", MachineID: "linux:fixture", Binary: filepath.Join(dir, "target", "bedrock")}, Profile: Profile{Version: 1, Host: host.Profile{Hostname: "fixture", SwapGiB: 0}, Bedrock: Binary{Version: "fixture-dev", SHA256: map[string]string{"linux_arm64": hex.EncodeToString(sum[:])}}}, Arch: "arm64", BinaryPath: source}
	def := Definition{Transport: remote.Transport{Program: []string{wrapper}}, CacheDir: filepath.Join(dir, "cache"), Probe: func(context.Context, remote.Target) (Facts, error) {
		return Facts{MachineID: "linux:fixture", Arch: "arm64", OS: "ubuntu", OSVersion: "24.04", Root: true, Systemd: true}, nil
	}}
	return def, in, record
}
func TestBootstrapPlansThenRunsPinnedProfileEndToEnd(t *testing.T) {
	def, in, record := bootstrapFixture(t)
	raw, _ := json.Marshal(in)
	plan, err := def.Plan(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 6 {
		t.Fatalf("steps %d", len(plan.Steps))
	}
	if _, err := os.Stat(in.Target.Binary); !os.IsNotExist(err) {
		t.Fatal("planning installed binary")
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatal("planning ran target commands")
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reg := kernel.Registry{}
	reg.Add(def)
	engine := kernel.New(store, reg, "bootstrap-test")
	receipt, err := engine.RunExpecting(context.Background(), Kind, raw, "", func(kernel.Event) {})
	if err != nil || receipt.Status != "succeeded" {
		t.Fatalf("receipt %+v err %v", receipt, err)
	}
	got, _ := os.ReadFile(record)
	for _, want := range []string{"capabilities --json", "version --json", "secret recipient", "host setup --hostname=fixture --swap=0 --timezone= --yes", "daemon install", "daemon status", "doctor"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
	original, _ := os.ReadFile(in.BinaryPath)
	installed, _ := os.ReadFile(in.Target.Binary)
	if !bytes.Equal(original, installed) {
		t.Fatal("installed different binary")
	}
	// Repeat after partial/retry execution remains safe and checksum-verified.
	if _, err := engine.RunExpecting(context.Background(), Kind, raw, "", func(kernel.Event) {}); err != nil {
		t.Fatal(err)
	}
}
func TestBootstrapRejectsIdentityChecksumAndExistingBuild(t *testing.T) {
	def, in, _ := bootstrapFixture(t)
	bad := in
	bad.Target.MachineID = "linux:other"
	raw, _ := json.Marshal(bad)
	if _, err := def.Plan(context.Background(), raw); err == nil {
		t.Fatal("changed machine accepted")
	}
	os.WriteFile(in.BinaryPath, []byte("different"), 0o600)
	raw, _ = json.Marshal(in)
	if _, err := def.Plan(context.Background(), raw); err == nil {
		t.Fatal("checksum mismatch accepted")
	}
	def, in, _ = bootstrapFixture(t)
	os.MkdirAll(filepath.Dir(in.Target.Binary), 0o700)
	os.WriteFile(in.Target.Binary, []byte("existing installation"), 0o700)
	raw, _ = json.Marshal(in)
	plan, err := def.Plan(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := plan.Steps[0].Apply(context.Background(), &out); err == nil {
		t.Fatal("overwrote existing different binary")
	}
	got, _ := os.ReadFile(in.Target.Binary)
	if string(got) != "existing installation" {
		t.Fatal("existing binary changed")
	}
}
func TestBootstrapReadinessFailureIsRecorded(t *testing.T) {
	def, in, _ := bootstrapFixture(t)
	t.Setenv("BOOTSTRAP_TEST_FAIL", "yes")
	raw, _ := json.Marshal(in)
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reg := kernel.Registry{}
	reg.Add(def)
	engine := kernel.New(store, reg, "test")
	receipt, err := engine.RunExpecting(context.Background(), Kind, raw, "", func(kernel.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status == "succeeded" || receipt.Error == "" {
		t.Fatalf("false readiness success: %+v", receipt)
	}
}
func TestProfilesRejectUnknownVersionFieldsAndUnpinnedBuilds(t *testing.T) {
	_, in, _ := bootstrapFixture(t)
	path := filepath.Join(t.TempDir(), "profile.json")
	if err := Save(path, in.Profile); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"version":2}`, `{"version":1,"unexpected":"x"}`, `{"version":1,"host":{"hostname":"fixture"},"bedrock":{"version":"1"}}`} {
		os.WriteFile(path, []byte(body), 0o600)
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestBootstrapApprovalBindsConnectionSettings(t *testing.T) {
	def, in, _ := bootstrapFixture(t)
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	reg := kernel.Registry{}
	reg.Add(def)
	engine := kernel.New(store, reg, "test")
	raw, _ := json.Marshal(in)
	first, err := engine.PlanOnly(context.Background(), Kind, raw)
	if err != nil {
		t.Fatal(err)
	}
	in.Target.Port = 2222
	raw, _ = json.Marshal(in)
	changed, err := engine.PlanOnly(context.Background(), Kind, raw)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest == changed.Digest {
		t.Fatal("connection change retained bootstrap approval")
	}
}

func TestBootstrapRejectsAnotherStoreWithTheSameSecretVersion(t *testing.T) {
	def, in, _ := bootstrapFixture(t)
	makeSource := func(host string) *secrets.Store {
		store := secrets.DefaultStore(t.TempDir())
		if _, _, _, err := store.EnsureKey(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := integration.Set(store, "email", map[string]string{"smtp_host": host, "smtp_port": "1025", "from": "fixture@example.com", "to": "owner@example.com"}); err != nil {
			t.Fatal(err)
		}
		return store
	}
	def.Secrets = makeSource("smtp.first.invalid")
	in.Profile.Integrations = []string{"email"}
	in.SecretsVersion, _ = def.Secrets.Current(integration.App)
	in.SecretsSHA256, _ = def.Secrets.SealedVersionSHA256(integration.App, in.SecretsVersion)
	raw, _ := json.Marshal(in)
	if _, err := def.Plan(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	def.Secrets = makeSource("smtp.second.invalid")
	version, _ := def.Secrets.Current(integration.App)
	if version != in.SecretsVersion {
		t.Fatal("fixture must use the same version number")
	}
	if _, err := def.Plan(context.Background(), raw); err == nil || !strings.Contains(err.Error(), "integration source changed") {
		t.Fatal("different source store accepted a pinned bootstrap")
	}
}
