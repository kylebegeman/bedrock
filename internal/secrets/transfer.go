package secrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"
)

var transferApp = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

const MaxTransferBytes = 64 * 1024

type transfer struct {
	App     string    `json:"app"`
	Name    string    `json:"name"`
	Value   string    `json:"value"`
	Expires time.Time `json:"expires"`
}

// Export seals exactly one secret for another machine. Only ciphertext leaves
// this store; the receiver must also specify the expected app and secret name.
func (s *Store) Export(from, name, to, recipient string) (string, error) {
	if !transferApp.MatchString(from) || !transferApp.MatchString(to) || from == "bedrock" || to == "bedrock" {
		return "", errors.New("integration credentials cannot be transferred")
	}
	if !ValidName(name) {
		return "", errors.New("invalid secret name")
	}
	r, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return "", errors.New("invalid receiving machine recipient")
	}
	values, _, err := s.LoadCurrent(from)
	if err != nil {
		return "", err
	}
	value, ok := values[name]
	if !ok {
		return "", fmt.Errorf("%s has no secret named %s", from, name)
	}
	data, err := json.Marshal(transfer{App: to, Name: name, Value: value, Expires: time.Now().Add(10 * time.Minute)})
	if err != nil {
		return "", errors.New("cannot encode secret transfer")
	}
	if len(data) > MaxTransferBytes/2 {
		return "", errors.New("secret is too large to transfer")
	}
	var buf bytes.Buffer
	armored := armor.NewWriter(&buf)
	w, err := age.Encrypt(armored, r)
	if err != nil {
		return "", err
	}
	if _, err = w.Write(data); err != nil {
		return "", err
	}
	if err = w.Close(); err != nil {
		return "", err
	}
	if err = armored.Close(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// Import accepts only authenticated ciphertext for this machine and the named
// destination. Parse failures deliberately never include decrypted bytes.
func (s *Store) Import(app, name string, input io.Reader) (int, error) {
	if !transferApp.MatchString(app) || app == "bedrock" || !ValidName(name) {
		return 0, errors.New("invalid secret transfer destination")
	}
	id, err := s.identity()
	if err != nil {
		return 0, err
	}
	encrypted, err := io.ReadAll(io.LimitReader(input, MaxTransferBytes+1))
	if err != nil || len(encrypted) > MaxTransferBytes {
		return 0, errors.New("invalid or oversized secret transfer")
	}
	r, err := age.Decrypt(armor.NewReader(bytes.NewReader(encrypted)), id)
	if err != nil {
		return 0, errors.New("secret transfer is not sealed for this machine")
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxTransferBytes+1))
	if err != nil || len(data) > MaxTransferBytes {
		return 0, errors.New("invalid secret transfer")
	}
	var t transfer
	if json.Unmarshal(data, &t) != nil || t.App != app || t.Name != name || !t.Expires.After(time.Now()) || t.Expires.After(time.Now().Add(11*time.Minute)) {
		return 0, errors.New("secret transfer is expired or does not match the destination")
	}
	current, version, err := s.LoadCurrent(app)
	if err != nil {
		return 0, err
	}
	if current[name] == t.Value {
		return version, nil
	}
	return s.Set(app, name, t.Value)
}
