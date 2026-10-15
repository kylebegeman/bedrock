package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kylebegeman/helios/internal/manifest"
	"github.com/kylebegeman/helios/internal/secrets"
)

func TestSecretPreflightAcceptsGeneratableNamesWithoutWritingSecrets(t *testing.T) {
	dir := t.TempDir()
	store := &secrets.Store{Dir: filepath.Join(dir, "secrets"), KeyPath: filepath.Join(dir, "key")}
	m, err := manifest.Parse([]byte("app: api\nworkloads: {worker: {kind: worker, image: alpine, secrets: [API_URL, DATABASE_URL, MINIO_ROOT_PASSWORD]}}\ndata: {postgres: {}, objects: {}}\nsecrets:\n  generate: {PASSWORD: 'hex:32'}\n  derive: {API_URL: 'postgres://api:{PASSWORD}@{postgres}/api'}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckDeploySecrets(m, store); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("secret preflight wrote state: %v %v", entries, err)
	}
}

func TestSecretPreflightNamesMissingWorkloadAndDatabaseInputs(t *testing.T) {
	for _, body := range []string{
		"app: api\nworkloads: {worker: {kind: worker, image: alpine, secrets: [API_TOKEN]}}\n",
		"app: api\nworkloads: {worker: {kind: worker, image: alpine}}\ndata: {postgres: {secrets: [API_TOKEN]}}\n",
		"app: api\nworkloads: {worker: {kind: worker, image: alpine}}\nchecks: [{url: 'https://api.example.com/', context: edge, headers_from_secrets: {Authorization: API_TOKEN}}]\n",
	} {
		m, err := manifest.Parse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckDeploySecrets(m, nil); err == nil || !strings.Contains(err.Error(), "API_TOKEN") {
			t.Fatalf("a missing secret passed preflight: %v", err)
		}
	}
}

func TestSecretPreflightRejectsDerivedDependencyCycles(t *testing.T) {
	m, err := manifest.Parse([]byte("app: api\nworkloads: {worker: {kind: worker, image: alpine, secrets: [FIRST]}}\nsecrets: {derive: {FIRST: '{SECOND}', SECOND: '{FIRST}'}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckDeploySecrets(m, nil); err == nil {
		t.Fatal("a derived-secret cycle passed preflight")
	}
}

func TestSecretPreflightReadsPreviewInheritanceWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	store := &secrets.Store{Dir: filepath.Join(dir, "secrets"), KeyPath: filepath.Join(dir, "key")}
	if _, _, _, err := store.EnsureKey(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Set("parent", "API_TOKEN", "private-sentinel"); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse([]byte("app: parent-pr-nav\nworkloads: {worker: {kind: worker, image: alpine, secrets: [DERIVED]}}\nsecrets: {derive: {DERIVED: '{API_TOKEN}'}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckDeploySecretsFrom(m, store, "parent"); err != nil {
		t.Fatal(err)
	}
	if current, err := store.Current(m.App); err != nil || current != 0 {
		t.Fatalf("preflight copied or generated secrets: %d %v", current, err)
	}
	if err := CheckDeploySecretsFrom(m, store, "missing-parent"); err == nil || strings.Contains(err.Error(), "private-sentinel") {
		t.Fatalf("missing inheritance must fail without revealing values: %v", err)
	}
}
