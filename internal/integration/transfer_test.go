package integration

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/bedrock/internal/secrets"
)

func transferStores(t *testing.T) (*secrets.Store, *secrets.Store, string) {
	t.Helper()
	one := &secrets.Store{Dir: filepath.Join(t.TempDir(), "secrets"), KeyPath: filepath.Join(t.TempDir(), "key")}
	two := &secrets.Store{Dir: filepath.Join(t.TempDir(), "secrets"), KeyPath: filepath.Join(t.TempDir(), "key")}
	if _, _, _, err := one.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	recipient, _, _, err := two.EnsureKey()
	if err != nil {
		t.Fatal(err)
	}
	return one, two, recipient
}
func seedTransfer(t *testing.T, s *secrets.Store) {
	t.Helper()
	if _, _, err := Set(s, "storage", map[string]string{"kind": "s3", "endpoint": "https://objects.example.com", "key_id": "fixture", "key": "sensitive-key", "bucket_prefix": "fixture", "password": "existing-backup-password"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Set(s, "loom", map[string]string{"verify_url": "https://auth.example.com/verify"}); err != nil {
		t.Fatal(err)
	}
}
func TestIntegrationTransferPreservesPasswordScopeAndIdentity(t *testing.T) {
	source, target, recipient := transferStores(t)
	seedTransfer(t, source)
	source.Set(App, "DEPLOY_KEY_API", "private operational key")
	sealed, err := Export(source, []string{"storage", "loom"}, recipient)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"sensitive-key", "existing-backup-password", "private operational key"} {
		if strings.Contains(sealed, secret) {
			t.Fatal("plaintext in transfer")
		}
	}
	stored, err := Import(target, []string{"loom", "storage"}, strings.NewReader(sealed), false)
	if err != nil || len(stored) != 2 {
		t.Fatalf("%v %v", stored, err)
	}
	got, err := Get(target, "storage")
	if err != nil || got["password"] != "existing-backup-password" {
		t.Fatalf("password changed: %v", err)
	}
	raw, version, err := target.LoadCurrent(App)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["DEPLOY_KEY_API"]; ok {
		t.Fatal("unselected operational key imported")
	}
	stored, err = Import(target, []string{"loom", "storage"}, strings.NewReader(sealed), false)
	_, same, _ := target.LoadCurrent(App)
	if err != nil || len(stored) != 0 || same != version {
		t.Fatal("import was not idempotent")
	}
	if _, err := source.ExportBundle(App, recipient); err == nil {
		t.Fatal("ordinary reserved-app transfer permitted")
	}
	if _, err := Import(target, []string{"storage"}, strings.NewReader(sealed), false); err == nil {
		t.Fatal("scope mismatch accepted")
	}
	_, other, otherRecipient := transferStores(t)
	_ = otherRecipient
	if _, err := Import(other, []string{"loom", "storage"}, strings.NewReader(sealed), false); err == nil {
		t.Fatal("wrong recipient accepted")
	}
}
func TestIntegrationTransferValidationIsAtomic(t *testing.T) {
	source, target, recipient := transferStores(t)
	seedTransfer(t, source)
	fields, _ := Get(source, "storage")
	fields["unknown"] = "do-not-log-this"
	bundle := transferBundle{Version: 1, Kind: "bedrock.integration-transfer", Expires: time.Now().Add(time.Minute), Names: []string{"loom", "storage"}, Values: map[string]map[string]string{"loom": {"verify_url": "https://auth.example.com/verify"}, "storage": fields}}
	raw, _ := json.Marshal(bundle)
	sealed, _ := secrets.SealDocument(recipient, raw)
	_, err := Import(target, bundle.Names, strings.NewReader(sealed), false)
	if err == nil || strings.Contains(err.Error(), "do-not-log-this") {
		t.Fatalf("unsafe validation: %v", err)
	}
	_, version, _ := target.LoadCurrent(App)
	if version != 0 {
		t.Fatal("invalid bundle partially changed target")
	}
	delete(fields, "unknown")
	bundle.Expires = time.Now().Add(-time.Second)
	raw, _ = json.Marshal(bundle)
	sealed, _ = secrets.SealDocument(recipient, raw)
	if _, err := Import(target, bundle.Names, strings.NewReader(sealed), false); err == nil {
		t.Fatal("expired transfer accepted")
	}
}
func TestIntegrationConflictAndRecoveryPreserveTargetConfiguration(t *testing.T) {
	source, target, recipient := transferStores(t)
	seedTransfer(t, source)
	seedTransfer(t, target)
	Set(target, "loom", map[string]string{"verify_url": "https://target.example.com/verify"})
	sealed, _ := Export(source, []string{"loom", "storage"}, recipient)
	_, version, _ := target.LoadCurrent(App)
	if _, err := Import(target, []string{"loom", "storage"}, bytes.NewBufferString(sealed), false); err == nil {
		t.Fatal("conflicting target replaced")
	}
	_, same, _ := target.LoadCurrent(App)
	if same != version {
		t.Fatal("conflict changed target")
	}
	if _, err := Import(target, []string{"loom", "storage"}, strings.NewReader(sealed), true); err != nil {
		t.Fatal(err)
	}
	Set(target, "loom", map[string]string{"verify_url": "https://target.example.com/verify"})
	target.Set(App, "DEPLOY_KEY_API", "target-key")
	source.Set(App, "DEPLOY_KEY_API", "source-key")
	source.Set(App, "WEBHOOK_SECRET_API", "source-hook")
	if _, err := RecoverMissing(target, source); err != nil {
		t.Fatal(err)
	}
	got, _ := Get(target, "loom")
	if got["verify_url"] != "https://target.example.com/verify" {
		t.Fatal("recovery changed target integration")
	}
	raw, _, _ := target.LoadCurrent(App)
	if raw["DEPLOY_KEY_API"] != "target-key" || raw["WEBHOOK_SECRET_API"] != "source-hook" {
		t.Fatal("recovery operational keys wrong")
	}
}
