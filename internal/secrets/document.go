package secrets

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// SealDocument encrypts a bounded, caller-validated configuration document.
// Ordinary app transfers keep their separate reserved-app restriction.
func SealDocument(recipient string, data []byte) (string, error) {
	if len(data) > MaxBundleBytes/2 {
		return "", errors.New("configuration document is too large")
	}
	r, err := parseRecipient(recipient)
	if err != nil {
		return "", err
	}
	return seal(data, r)
}

// OpenDocument authenticates ciphertext using this machine's existing identity.
// Errors never contain decrypted document bytes.
func (s *Store) OpenDocument(input io.Reader) ([]byte, error) {
	return s.unseal(input, MaxBundleBytes, "configuration document")
}

// SealedVersionSHA256 identifies immutable encrypted bytes without disclosing
// secret values or relying on version numbers unique only within one store.
func (s *Store) SealedVersionSHA256(app string, version int) (string, error) {
	if !appPattern.MatchString(app) || version <= 0 {
		return "", errors.New("sealed version needs a valid app and positive version")
	}
	f, err := os.Open(filepath.Join(s.appDir(app), fmt.Sprintf("v%d.age", version)))
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum := sha256.New()
	n, err := io.Copy(sum, io.LimitReader(f, MaxBundleBytes+1))
	if err != nil {
		return "", err
	}
	if n > MaxBundleBytes {
		return "", errors.New("sealed secret version is too large")
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}
