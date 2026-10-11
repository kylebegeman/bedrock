package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/kylebegeman/bedrock/internal/secrets"
)

var recoverySourcePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

var recoveryIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{7,47}$`)
var recoveryAppPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)
var recoveryVersionFile = regexp.MustCompile(`^v([1-9][0-9]*)\.age$`)

const maxRecoveryIdentity = 64 << 10
const maxRecoverySecretBundle = 16 << 20

// RecoveryID consistently identifies a recovery of an exact requested
// snapshot. Rehearsal and real recovery always have separate workspaces.
func RecoveryID(source, snapshot string, rehearsal bool) string {
	mode := "recover"
	if rehearsal {
		mode = "rehearse"
	}
	sum := sha256.Sum256([]byte(mode + "\n" + source + "\n" + snapshot))
	return mode + "-" + hex.EncodeToString(sum[:8])
}

func RecoveryDirectory(stateDir, id string) string {
	return filepath.Join(stateDir, "machine-recovery", id)
}

func recoveryPlainFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("recovery input must be a plain file")
	}
	return nil
}

func parseRecoveryIdentity(data []byte) (*age.X25519Identity, error) {
	if len(data) > maxRecoveryIdentity {
		return nil, errors.New("recovery identity is too large")
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "AGE-SECRET-KEY-") {
			id, err := age.ParseX25519Identity(line)
			if err != nil {
				return nil, errors.New("invalid recovery identity")
			}
			return id, nil
		}
	}
	return nil, errors.New("the recovery identity file contains no age identity")
}

func targetRecoveryIdentity(sec *secrets.Store) (*age.X25519Identity, error) {
	if err := recoveryPlainFile(sec.KeyPath); err != nil {
		return nil, errors.New("the target needs its own secrets identity; run bedrock init first")
	}
	data, err := os.ReadFile(sec.KeyPath)
	if err != nil {
		return nil, err
	}
	return parseRecoveryIdentity(data)
}

// SealRecoveryIdentity protects a source identity with the target's own
// identity before the operation is journaled. Neither raw keys nor their
// ciphertext belong in operation inputs, receipts, arguments or output.
func SealRecoveryIdentity(sec *secrets.Store, stateDir, id string, input io.Reader) (string, error) {
	if !recoveryIDPattern.MatchString(id) {
		return "", errors.New("invalid recovery ID")
	}
	plain, err := io.ReadAll(io.LimitReader(input, maxRecoveryIdentity+1))
	if err != nil {
		return "", err
	}
	source, err := parseRecoveryIdentity(plain)
	if err != nil {
		return "", err
	}
	target, err := targetRecoveryIdentity(sec)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(struct{ RecoveryID, Identity string }{id, source.String()})
	if err != nil {
		return "", errors.New("cannot prepare recovery identity")
	}
	var encrypted bytes.Buffer
	w, err := age.Encrypt(&encrypted, target.Recipient())
	if err != nil {
		return "", errors.New("cannot seal recovery identity")
	}
	if _, err := w.Write(data); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	dir := RecoveryDirectory(stateDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("recovery workspace must be a plain directory")
	}
	path := filepath.Join(dir, "identity.age")
	// Reusing a sealed identity keeps an already approved input digest
	// stable and prevents a resumed operation from following a different key.
	if old, err := os.ReadFile(path); err == nil {
		oldHash := sha256.Sum256(old)
		oldSource, err := openRecoveryIdentity(sec, stateDir, id, hex.EncodeToString(oldHash[:]))
		if err != nil {
			return "", err
		}
		if oldSource.String() != source.String() {
			return "", errors.New("this recovery already has a different source identity")
		}
		return hex.EncodeToString(oldHash[:]), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// Once all source secrets are resealed, later steps no longer need
	// the source identity. Keep the originally journaled digest on retry
	// instead of recreating a removed artifact with new random ciphertext.
	recordPath := filepath.Join(dir, "recovery-record.json")
	if err := recoveryPlainFile(recordPath); err == nil {
		data, err := os.ReadFile(recordPath)
		if err != nil {
			return "", err
		}
		var rec machineRecoveryRecord
		if json.Unmarshal(data, &rec) != nil || rec.Input.ID != id {
			return "", errors.New("invalid recovery record")
		}
		if rec.SecretsReady && fullSnapshotID.MatchString(rec.Input.IdentitySHA256) {
			if rec.Catalog.Recipient != source.Recipient().String() {
				return "", errors.New("this recovery already has a different source identity")
			}
			return rec.Input.IdentitySHA256, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(encrypted.Bytes()); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	// The protected artifact and its workspace must survive before its
	// checksum is admitted into the operation journal.
	for _, path := range []string{dir, filepath.Dir(dir), stateDir} {
		if err := syncRecoveryDirectory(path); err != nil {
			return "", err
		}
	}
	sum := sha256.Sum256(encrypted.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

// RecoveryIdentityHash lets a retry refer to the previously sealed key.
func RecoveryIdentityHash(stateDir, id string) (string, error) {
	if !recoveryIDPattern.MatchString(id) {
		return "", errors.New("invalid recovery ID")
	}
	path := filepath.Join(RecoveryDirectory(stateDir, id), "identity.age")
	if err := recoveryPlainFile(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			data, readErr := os.ReadFile(filepath.Join(RecoveryDirectory(stateDir, id), "recovery-record.json"))
			var rec machineRecoveryRecord
			if readErr == nil && json.Unmarshal(data, &rec) == nil && rec.Input.ID == id && rec.SecretsReady && fullSnapshotID.MatchString(rec.Input.IdentitySHA256) {
				return rec.Input.IdentitySHA256, nil
			}
		}
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func openRecoveryIdentity(sec *secrets.Store, stateDir, id, expected string) (*age.X25519Identity, error) {
	path := filepath.Join(RecoveryDirectory(stateDir, id), "identity.age")
	if err := recoveryPlainFile(path); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("sealed recovery identity must be readable only by its owner")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if !fullSnapshotID.MatchString(expected) || hex.EncodeToString(sum[:]) != expected {
		return nil, errors.New("sealed recovery identity checksum changed")
	}
	target, err := targetRecoveryIdentity(sec)
	if err != nil {
		return nil, err
	}
	r, err := age.Decrypt(bytes.NewReader(data), target)
	if err != nil {
		return nil, errors.New("cannot open the sealed recovery identity with the target identity")
	}
	plain, err := io.ReadAll(io.LimitReader(r, maxRecoveryIdentity+1))
	if err != nil {
		return nil, errors.New("cannot read the sealed recovery identity")
	}
	var envelope struct{ RecoveryID, Identity string }
	if json.Unmarshal(plain, &envelope) != nil || envelope.RecoveryID != id {
		return nil, errors.New("sealed recovery identity belongs to a different recovery")
	}
	return parseRecoveryIdentity([]byte(envelope.Identity))
}

// resealRecoverySecrets decrypts only in memory and writes each source
// version sealed to the target. Preserved numbers keep historical rollback
// and revision secret references meaningful.
func resealRecoverySecrets(sourceDir, destination string, source, target *age.X25519Identity) (map[string]map[int]int, error) {
	entries, err := os.ReadDir(sourceDir)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]map[int]int{}, nil
	}
	if err != nil {
		return nil, err
	}
	mapping := map[string]map[int]int{}
	for _, entry := range entries {
		app := entry.Name()
		if !entry.IsDir() || !recoveryAppPattern.MatchString(app) {
			return nil, errors.New("machine snapshot contains an invalid sealed app directory")
		}
		dir := filepath.Join(sourceDir, app)
		files, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		stage, err := os.MkdirTemp(filepath.Dir(destination), ".reseal-*")
		if err != nil {
			return nil, err
		}
		cleanup := func() { _ = os.RemoveAll(stage) }
		versions := map[int]int{}
		for _, file := range files {
			match := recoveryVersionFile.FindStringSubmatch(file.Name())
			if match == nil {
				continue
			}
			path := filepath.Join(dir, file.Name())
			if err := recoveryPlainFile(path); err != nil {
				cleanup()
				return nil, err
			}
			ciphertext, err := os.ReadFile(path)
			if err != nil {
				cleanup()
				return nil, err
			}
			r, err := age.Decrypt(bytes.NewReader(ciphertext), source)
			if err != nil {
				cleanup()
				return nil, fmt.Errorf("the recovery identity cannot open %s's secrets", app)
			}
			plain, err := io.ReadAll(io.LimitReader(r, maxRecoverySecretBundle+1))
			if err != nil || len(plain) > maxRecoverySecretBundle {
				cleanup()
				return nil, errors.New("cannot read the source sealed secret bundle")
			}
			var bundle struct {
				Version int               `json:"version"`
				At      time.Time         `json:"at"`
				Values  map[string]string `json:"values"`
			}
			version, _ := strconv.Atoi(match[1])
			if json.Unmarshal(plain, &bundle) != nil || bundle.Version != version {
				cleanup()
				return nil, errors.New("source secret bundle has an invalid version")
			}
			names := make([]string, 0, len(bundle.Values))
			for name := range bundle.Values {
				if !secrets.ValidName(name) {
					cleanup()
					return nil, errors.New("source secret bundle contains an invalid name")
				}
				names = append(names, name)
			}
			slices.Sort(names)
			var sealed bytes.Buffer
			w, err := age.Encrypt(&sealed, target.Recipient())
			if err != nil {
				cleanup()
				return nil, err
			}
			if _, err := w.Write(plain); err != nil {
				cleanup()
				return nil, err
			}
			if err := w.Close(); err != nil {
				cleanup()
				return nil, err
			}
			if err := writeRecoveryFile(filepath.Join(stage, file.Name()), sealed.Bytes()); err != nil {
				cleanup()
				return nil, err
			}
			if err := writeRecoveryJSON(filepath.Join(stage, fmt.Sprintf("v%d.meta", version)), secrets.VersionInfo{Version: version, At: bundle.At, Names: names}); err != nil {
				cleanup()
				return nil, err
			}
			versions[version] = version
		}
		currentFile := filepath.Join(dir, "current")
		if err := recoveryPlainFile(currentFile); err != nil {
			cleanup()
			return nil, err
		}
		currentBytes, err := os.ReadFile(currentFile)
		if err != nil {
			cleanup()
			return nil, err
		}
		current, err := strconv.Atoi(strings.TrimSpace(string(currentBytes)))
		if err != nil || current < 1 || versions[current] != current {
			cleanup()
			return nil, errors.New("source current secret version is missing")
		}
		if err := writeRecoveryFile(filepath.Join(stage, "current"), []byte(strconv.Itoa(current)+"\n")); err != nil {
			cleanup()
			return nil, err
		}
		if err := os.MkdirAll(destination, 0o700); err != nil {
			cleanup()
			return nil, err
		}
		dest := filepath.Join(destination, app)
		if _, err := os.Lstat(dest); err == nil {
			// Workspace versions are disposable derived copies. Live target
			// versions are installed separately only after clean-target checks.
			if err := os.RemoveAll(dest); err != nil {
				cleanup()
				return nil, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			cleanup()
			return nil, err
		}
		if err := os.Rename(stage, dest); err != nil {
			cleanup()
			return nil, err
		}
		if err := syncRecoveryDirectory(destination); err != nil {
			return nil, err
		}
		mapping[app] = versions
	}
	return mapping, nil
}
