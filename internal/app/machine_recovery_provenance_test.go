package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/helios/internal/kernel"
	"github.com/kylebegeman/helios/internal/manifest"
	"github.com/kylebegeman/helios/internal/state"
)

func provenanceFixture(t *testing.T) (state.Revision, appBackupProvenance) {
	t.Helper()
	m := recoveryManifest()
	m.Data = &manifest.Data{Volumes: map[string]manifest.Volume{"uploads": {}}}
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	rev := state.Revision{App: m.App, ID: "current-code", Manifest: body, SecretsVersion: 3,
		Images: map[string]string{"web": "sha256:" + strings.Repeat("b", 64)}, CreatedAt: time.Now().UTC().Add(-time.Hour)}
	return rev, newAppBackupProvenance(rev, rev.CreatedAt.Add(time.Minute))
}

func TestRecoveryProvenanceRejectsOldCodeEvenWhenLayoutIsIdentical(t *testing.T) {
	rev, good := provenanceFixture(t)
	cases := map[string]appBackupProvenance{"missing": {}}
	old := good
	old.Revision = "before-migration"
	cases["older revision with identical manifest"] = old
	old = good
	old.SecretsVersion--
	cases["different secret version"] = old
	old = good
	old.ManifestSHA256 = strings.Repeat("a", 64)
	cases["different manifest"] = old
	old = good
	old.CapturedAt = rev.CreatedAt.Add(-time.Minute)
	cases["data predates active revision"] = old
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			catalog := machineRecoveryCatalog{Revisions: []state.Revision{rev}, AppSnapshots: map[string]string{rev.App: strings.Repeat("c", 64)}, AppData: map[string]appBackupProvenance{rev.App: p}}
			if err := validateRecoveryCatalog(catalog); err == nil || !strings.Contains(err.Error(), "helios backup hello") {
				t.Fatalf("unproven data recovery point accepted: %v", err)
			}
		})
	}
	catalog := machineRecoveryCatalog{Revisions: []state.Revision{rev}, AppSnapshots: map[string]string{rev.App: strings.Repeat("c", 64)}, AppData: map[string]appBackupProvenance{rev.App: good}}
	if err := validateRecoveryCatalog(catalog); err != nil {
		t.Fatal("exact revision and secrets recovery point refused:", err)
	}
}

func TestRecoveryProvenanceMustMatchTheManifestActuallyBackedUp(t *testing.T) {
	rev, good := provenanceFixture(t)
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifestPath, append(rev.Manifest, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeAppBackupProvenance(dir, rev, good.CapturedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := readAppBackupProvenance(dir, rev); err != nil {
		t.Fatal(err)
	}
	var old map[string]any
	if err := json.Unmarshal(rev.Manifest, &old); err != nil {
		t.Fatal(err)
	}
	old["description"] = "manifest from a previous deployment"
	body, _ := json.Marshal(old)
	if err := os.WriteFile(manifestPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readAppBackupProvenance(dir, rev); err == nil {
		t.Fatal("provenance did not detect a stale snapshot manifest")
	}
	if err := writeAppBackupProvenance(dir, rev, good.CapturedAt); err == nil {
		t.Fatal("an interrupted backup relabeled a stale manifest as current code")
	}
}

func TestRecoveryAppBackupApprovalBindsTheActiveRevision(t *testing.T) {
	r, _ := recoveryFixture(t)
	rev, _ := provenanceFixture(t)
	ctx := context.Background()
	if err := r.Store.SaveRevision(ctx, rev); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.Activate(ctx, rev.App, rev.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	reg := kernel.Registry{}
	reg.Add(Backup{Store: r.Store, Secrets: r.Secrets, StateDir: r.StateDir})
	engine := kernel.New(r.Store, reg, "backup-provenance-test")
	raw, _ := json.Marshal(BackupInput{App: rev.App})
	view, err := engine.PlanOnly(ctx, BackupKind, raw)
	if err != nil {
		t.Fatal(err)
	}
	// An interrupted backup cannot rebuild itself around a newer deployment
	// while adopting the previous deployment's already prepared manifest.
	rev.ID = "next-code"
	if err := r.Store.SaveRevision(ctx, rev); err != nil {
		t.Fatal(err)
	}
	if err := r.Store.Activate(ctx, rev.App, rev.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.RunExpecting(ctx, BackupKind, raw, view.Digest, func(kernel.Event) {}); !errors.Is(err, kernel.ErrPlanChanged) {
		t.Fatalf("stale backup approval reached apply: %v", err)
	}
	ops, err := r.Store.List(ctx, 10)
	if err != nil || len(ops) != 0 {
		t.Fatal("refused stale backup was journaled or applied")
	}
}
