package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kylebegeman/bedrock/internal/state"
)

const appBackupProvenanceFile = "backup-revision.json"
const appBackupProvenanceFormat = 1

// appBackupProvenance binds quiesced data to the code and secret version
// that produced it. Volume/service layout alone cannot prove compatibility.
type appBackupProvenance struct {
	Format         int       `json:"format"`
	App            string    `json:"app"`
	Revision       string    `json:"revision"`
	SecretsVersion int       `json:"secrets_version"`
	ManifestSHA256 string    `json:"manifest_sha256"`
	CapturedAt     time.Time `json:"captured_at"`
}

func backupManifestHash(data []byte) string {
	var compact bytes.Buffer
	if json.Compact(&compact, data) == nil {
		data = compact.Bytes()
	}
	sum := sha256.Sum256(bytes.TrimSpace(data))
	return hex.EncodeToString(sum[:])
}

func newAppBackupProvenance(rev state.Revision, captured time.Time) appBackupProvenance {
	return appBackupProvenance{Format: appBackupProvenanceFormat, App: rev.App, Revision: rev.ID,
		SecretsVersion: rev.SecretsVersion, ManifestSHA256: backupManifestHash(rev.Manifest), CapturedAt: captured.UTC()}
}

func freshRecoveryBackupRequired(rev state.Revision) error {
	return fmt.Errorf("%s's data backup does not prove compatibility with active revision %s and secret version %d; run bedrock backup %s before backing up the whole machine", rev.App, rev.ID, rev.SecretsVersion, rev.App)
}

func validateAppBackupProvenance(rev state.Revision, p appBackupProvenance) error {
	if p.Format != appBackupProvenanceFormat || p.App != rev.App || p.Revision != rev.ID ||
		p.SecretsVersion != rev.SecretsVersion || p.ManifestSHA256 != backupManifestHash(rev.Manifest) ||
		p.CapturedAt.IsZero() || p.CapturedAt.Before(rev.CreatedAt) {
		return freshRecoveryBackupRequired(rev)
	}
	return nil
}

func readAppBackupProvenance(dir string, rev state.Revision) (appBackupProvenance, error) {
	var p appBackupProvenance
	path := filepath.Join(dir, appBackupProvenanceFile)
	if err := recoveryPlainFile(path); err != nil {
		return p, freshRecoveryBackupRequired(rev)
	}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &p) != nil {
		return p, freshRecoveryBackupRequired(rev)
	}
	if err := validateAppBackupProvenance(rev, p); err != nil {
		return p, err
	}
	manifest, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil || backupManifestHash(manifest) != p.ManifestSHA256 {
		return p, freshRecoveryBackupRequired(rev)
	}
	return p, nil
}

func writeAppBackupProvenance(dir string, rev state.Revision, captured time.Time) error {
	manifest, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return err
	}
	if backupManifestHash(manifest) != backupManifestHash(rev.Manifest) {
		return errors.New("the active revision changed after this backup was prepared; start a fresh app backup")
	}
	p := newAppBackupProvenance(rev, captured)
	if err := validateAppBackupProvenance(rev, p); err != nil {
		return err
	}
	return writeRecoveryJSON(filepath.Join(dir, appBackupProvenanceFile), p)
}
