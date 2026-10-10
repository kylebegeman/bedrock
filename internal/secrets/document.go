package secrets

import (
	"errors"
	"io"
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
