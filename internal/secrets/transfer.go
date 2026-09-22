package secrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
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

// MaxBundleBytes bounds a whole app's secrets. An app with more than this
// is not moving its configuration, it is moving its data, and data belongs
// in a volume.
const MaxBundleBytes = 1 << 20

// timeNow is the clock the bundle expiry reads, so a test can prove a
// bundle stops working rather than waiting ten minutes to find out.
var timeNow = time.Now

type secretBundle struct {
	App     string            `json:"app"`
	Values  map[string]string `json:"values"`
	Expires time.Time         `json:"expires"`
}

// ExportBundle seals every secret an app holds for another machine, so an
// app can be moved without opening the sealed store by hand.
//
// A moved app must arrive with the secrets it left with. Its database was
// restored from a snapshot and still holds the role password the source
// generated, so a target that generated its own would be handed a database
// it cannot open. Sealing to the target's recipient is what lets that
// travel without the values passing through a terminal, a log or a shell
// history on the way.
//
// The bundle expires quickly for the same reason a single transfer does: it
// is meant to be produced and consumed in the same sitting, not kept.
func (s *Store) ExportBundle(app, recipient string) (string, error) {
	if !transferApp.MatchString(app) || app == "bedrock" {
		return "", errors.New("integration credentials cannot be transferred")
	}
	r, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return "", errors.New("invalid receiving machine recipient")
	}
	values, _, err := s.LoadCurrent(app)
	if err != nil {
		return "", err
	}
	if len(values) == 0 {
		return "", fmt.Errorf("%s holds no secrets", app)
	}
	data, err := json.Marshal(secretBundle{App: app, Values: values, Expires: timeNow().Add(10 * time.Minute)})
	if err != nil {
		return "", errors.New("cannot encode secret bundle")
	}
	if len(data) > MaxBundleBytes/2 {
		return "", fmt.Errorf("%s's secrets are too large to transfer", app)
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

// ImportBundle accepts only authenticated ciphertext sealed for this machine
// and naming this app. It returns the names it stored, never the values, and
// parse failures deliberately never include decrypted bytes.
//
// A name already holding the same value is left alone rather than written
// again, so importing twice does not churn the version history.
func (s *Store) ImportBundle(app string, input io.Reader) ([]string, error) {
	if !transferApp.MatchString(app) || app == "bedrock" {
		return nil, errors.New("invalid secret transfer destination")
	}
	id, err := s.identity()
	if err != nil {
		return nil, err
	}
	encrypted, err := io.ReadAll(io.LimitReader(input, MaxBundleBytes+1))
	if err != nil || len(encrypted) > MaxBundleBytes {
		return nil, errors.New("invalid or oversized secret bundle")
	}
	r, err := age.Decrypt(armor.NewReader(bytes.NewReader(encrypted)), id)
	if err != nil {
		return nil, errors.New("secret bundle is not sealed for this machine")
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxBundleBytes+1))
	if err != nil || len(data) > MaxBundleBytes {
		return nil, errors.New("invalid secret bundle")
	}
	var b secretBundle
	if json.Unmarshal(data, &b) != nil || b.App != app || !b.Expires.After(timeNow()) || b.Expires.After(timeNow().Add(11*time.Minute)) {
		return nil, errors.New("secret bundle is expired or does not match the destination")
	}
	current, _, err := s.LoadCurrent(app)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(b.Values))
	for name := range b.Values {
		names = append(names, name)
	}
	sort.Strings(names)
	var stored []string
	for _, name := range names {
		if !ValidName(name) {
			return stored, fmt.Errorf("the bundle names a secret that isn't one: %q", name)
		}
		if current[name] == b.Values[name] {
			continue
		}
		if _, err := s.Set(app, name, b.Values[name]); err != nil {
			return stored, err
		}
		stored = append(stored, name)
	}
	return stored, nil
}

// CopyAll copies every secret an app holds to another app on this machine,
// and returns the names it wrote.
//
// This is for a preview, which runs the parent app's code and so needs the
// hand-set values that code expects. Anything the target already holds with
// the same value is left alone, so running it again writes no new version.
//
// It stays on one machine and never touches bedrock's own integration
// credentials, which belong to the machine rather than to any app.
func (s *Store) CopyAll(from, to string) ([]string, error) {
	if !transferApp.MatchString(from) || !transferApp.MatchString(to) || from == "bedrock" || to == "bedrock" {
		return nil, errors.New("integration credentials cannot be copied")
	}
	if from == to {
		return nil, errors.New("an app already has its own secrets")
	}
	values, _, err := s.LoadCurrent(from)
	if err != nil {
		return nil, err
	}
	current, _, err := s.LoadCurrent(to)
	if err != nil {
		return nil, err
	}
	changes := map[string]*string{}
	names := make([]string, 0, len(values))
	for name, value := range values {
		if !ValidName(name) || current[name] == value {
			continue
		}
		v := value
		changes[name] = &v
		names = append(names, name)
	}
	if len(changes) == 0 {
		return nil, nil
	}
	sort.Strings(names)
	if _, err := s.SetAll(to, changes); err != nil {
		return nil, err
	}
	return names, nil
}
