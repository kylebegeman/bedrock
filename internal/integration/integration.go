// Package integration keeps the credentials quark itself uses, such as
// the object storage backups go to and the mail server alerts go through,
// in the machine's secrets store under quark's own name. Each integration
// is a named set of fields; values are typed in once and never shown.
package integration

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kylebegeman/quark/internal/manifest"
	"github.com/kylebegeman/quark/internal/secrets"
)

// App is the reserved app whose secrets hold the integrations.
const App = manifest.ReservedApp

// Field is one value an integration needs.
type Field struct {
	// Name is lower_snake; the secret is stored as INTEGRATION_NAME.
	Name string
	// Prompt is what to ask for.
	Prompt string
	// Secret values are typed without echo and never listed.
	Secret bool
	// Optional fields may stay empty.
	Optional bool
	// Default applies when the value is empty.
	Default string
	// Choices limits the value to one of these.
	Choices []string
}

// Definition is one integration quark knows how to use.
type Definition struct {
	Name    string
	Purpose string
	Fields  []Field
}

// Names of the integrations.
const (
	StorageName    = "storage"
	EmailName      = "email"
	CloudflareName = "cloudflare"
)

// Definitions lists every integration, in the order they are shown.
var Definitions = []Definition{
	{
		Name:    StorageName,
		Purpose: "object storage for backups, one bucket per app",
		Fields: []Field{
			{Name: "kind", Prompt: "b2 (Backblaze) or s3 (any S3-compatible store)", Default: "b2", Choices: []string{"b2", "s3"}},
			{Name: "endpoint", Prompt: "S3 endpoint URL, for s3 only", Optional: true},
			{Name: "key_id", Prompt: "key ID"},
			{Name: "key", Prompt: "key", Secret: true},
			{Name: "bucket_prefix", Prompt: "bucket name prefix, such as kb (buckets are prefix-app)"},
			{Name: "password", Prompt: "backup encryption password; leave blank to make one, then keep it with the recovery identity", Secret: true, Optional: true},
		},
	},
	{
		Name:    EmailName,
		Purpose: "alerts by email",
		Fields: []Field{
			{Name: "smtp_host", Prompt: "SMTP host"},
			{Name: "smtp_port", Prompt: "SMTP port (465 for TLS, 587 for STARTTLS)", Default: "587"},
			{Name: "smtp_user", Prompt: "SMTP user", Optional: true},
			{Name: "smtp_password", Prompt: "SMTP password", Secret: true, Optional: true},
			{Name: "from", Prompt: "from address"},
			{Name: "to", Prompt: "to address (several separated by commas)"},
		},
	},
	{
		Name:    CloudflareName,
		Purpose: "DNS records for the apps' hostnames",
		Fields: []Field{
			{Name: "token", Prompt: "API token with DNS edit on the zones", Secret: true},
		},
	},
}

// Lookup finds an integration by name.
func Lookup(name string) (Definition, bool) {
	for _, d := range Definitions {
		if d.Name == name {
			return d, true
		}
	}
	return Definition{}, false
}

// Names lists the integration names.
func Names() []string {
	names := make([]string, 0, len(Definitions))
	for _, d := range Definitions {
		names = append(names, d.Name)
	}
	return names
}

func (d Definition) key(field string) string {
	return strings.ToUpper(d.Name + "_" + field)
}

// ErrNotSet means an integration hasn't been set up on this machine.
var ErrNotSet = errors.New("not set up")

var bucketPrefixPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,19}$`)

// Set stores an integration's values in one new version of quark's
// secrets and returns values it made up, such as a generated password,
// so the caller can show them exactly once.
func Set(store *secrets.Store, name string, values map[string]string) (version int, generated map[string]string, err error) {
	def, ok := Lookup(name)
	if !ok {
		return 0, nil, fmt.Errorf("no integration named %q; there are %s", name, strings.Join(Names(), ", "))
	}
	known := map[string]bool{}
	for _, f := range def.Fields {
		known[f.Name] = true
	}
	for k := range values {
		if !known[k] {
			return 0, nil, fmt.Errorf("%s has no field named %q", name, k)
		}
	}
	generated = map[string]string{}
	changes := map[string]*string{}
	for _, f := range def.Fields {
		v := strings.TrimSpace(values[f.Name])
		if v == "" {
			v = f.Default
		}
		if v == "" && name == StorageName && f.Name == "password" {
			var b [24]byte
			if _, err := rand.Read(b[:]); err != nil {
				return 0, nil, err
			}
			v = hex.EncodeToString(b[:])
			generated[f.Name] = v
		}
		if v == "" {
			if !f.Optional {
				return 0, nil, fmt.Errorf("%s needs %s (%s)", name, f.Name, f.Prompt)
			}
			changes[def.key(f.Name)] = nil
			continue
		}
		if len(f.Choices) > 0 {
			match := false
			for _, c := range f.Choices {
				if c == v {
					match = true
				}
			}
			if !match {
				return 0, nil, fmt.Errorf("%s.%s must be one of %s", name, f.Name, strings.Join(f.Choices, ", "))
			}
		}
		value := v
		changes[def.key(f.Name)] = &value
	}
	if err := validate(name, fieldValues(def, changes)); err != nil {
		return 0, nil, err
	}
	version, err = store.SetAll(App, changes)
	return version, generated, err
}

func fieldValues(def Definition, changes map[string]*string) map[string]string {
	out := map[string]string{}
	for _, f := range def.Fields {
		if v := changes[def.key(f.Name)]; v != nil {
			out[f.Name] = *v
		}
	}
	return out
}

func validate(name string, v map[string]string) error {
	switch name {
	case StorageName:
		if v["kind"] == "s3" {
			if !strings.HasPrefix(v["endpoint"], "http://") && !strings.HasPrefix(v["endpoint"], "https://") {
				return errors.New("storage.endpoint must be the S3 endpoint URL, starting with https:// or http://")
			}
		} else if v["endpoint"] != "" {
			return errors.New("storage.endpoint is for s3 only; Backblaze needs none")
		}
		if !bucketPrefixPattern.MatchString(v["bucket_prefix"]) {
			return errors.New("storage.bucket_prefix must be 2 to 20 lowercase letters, digits and hyphens")
		}
	case EmailName:
		port, err := strconv.Atoi(v["smtp_port"])
		if err != nil || port < 1 || port > 65535 {
			return errors.New("email.smtp_port must be a port number")
		}
		for _, f := range []string{"from", "to"} {
			for _, addr := range strings.Split(v[f], ",") {
				if !strings.Contains(strings.TrimSpace(addr), "@") {
					return fmt.Errorf("email.%s: %q isn't an address", f, strings.TrimSpace(addr))
				}
			}
		}
		if (v["smtp_user"] == "") != (v["smtp_password"] == "") {
			return errors.New("email: give both smtp_user and smtp_password, or neither")
		}
	}
	return nil
}

// Get returns an integration's values by field name. ErrNotSet when it
// isn't set up.
func Get(store *secrets.Store, name string) (map[string]string, error) {
	def, ok := Lookup(name)
	if !ok {
		return nil, fmt.Errorf("no integration named %q", name)
	}
	all, _, err := store.LoadCurrent(App)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, f := range def.Fields {
		if v, ok := all[def.key(f.Name)]; ok {
			out[f.Name] = v
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: %w; run quark integration set %s", name, ErrNotSet, name)
	}
	return out, nil
}

// Remove drops an integration's values.
func Remove(store *secrets.Store, name string) (int, error) {
	def, ok := Lookup(name)
	if !ok {
		return 0, fmt.Errorf("no integration named %q", name)
	}
	if _, err := Get(store, name); err != nil {
		return 0, err
	}
	changes := map[string]*string{}
	for _, f := range def.Fields {
		changes[def.key(f.Name)] = nil
	}
	return store.SetAll(App, changes)
}

// Status is an integration as listed: which fields are set, never the
// secret values.
type Status struct {
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
	Set     bool   `json:"set"`
	// At is when the current values were stored.
	At time.Time `json:"at,omitempty"`
	// Fields holds plain values, and "(set)" for secrets.
	Fields map[string]string `json:"fields,omitempty"`
}

// List describes every integration.
func List(store *secrets.Store) ([]Status, error) {
	all, _, err := store.LoadCurrent(App)
	if err != nil {
		return nil, err
	}
	versions, err := store.Versions(App)
	if err != nil {
		return nil, err
	}
	var out []Status
	for _, d := range Definitions {
		st := Status{Name: d.Name, Purpose: d.Purpose, Fields: map[string]string{}}
		for _, f := range d.Fields {
			v, ok := all[d.key(f.Name)]
			if !ok {
				continue
			}
			st.Set = true
			if f.Secret {
				st.Fields[f.Name] = "(set)"
			} else {
				st.Fields[f.Name] = v
			}
		}
		if st.Set {
			st.At = lastChange(versions, d)
		}
		out = append(out, st)
	}
	return out, nil
}

// lastChange finds when an integration's names last appeared in a new
// version: the newest version whose set of the integration's names
// differs from the one before it is when it was last set.
func lastChange(versions []secrets.VersionInfo, d Definition) time.Time {
	names := func(v secrets.VersionInfo) string {
		var mine []string
		for _, n := range v.Names {
			if strings.HasPrefix(n, strings.ToUpper(d.Name)+"_") {
				mine = append(mine, n)
			}
		}
		sort.Strings(mine)
		return strings.Join(mine, ",")
	}
	// versions are newest first
	for i := 0; i < len(versions); i++ {
		if i+1 == len(versions) || names(versions[i]) != names(versions[i+1]) {
			return versions[i].At
		}
	}
	return time.Time{}
}

// Storage is the object storage backups go to.
type Storage struct {
	Kind         string
	Endpoint     string
	KeyID        string
	Key          string
	BucketPrefix string
	Password     string
}

// LoadStorage reads the storage integration.
func LoadStorage(store *secrets.Store) (*Storage, error) {
	v, err := Get(store, StorageName)
	if err != nil {
		return nil, err
	}
	return &Storage{Kind: v["kind"], Endpoint: v["endpoint"], KeyID: v["key_id"], Key: v["key"], BucketPrefix: v["bucket_prefix"], Password: v["password"]}, nil
}

// Bucket names an app's bucket.
func (s Storage) Bucket(app string) string { return s.BucketPrefix + "-" + app }

// Repository is the restic repository string for a bucket.
func (s Storage) Repository(bucket string) string {
	if s.Kind == "s3" {
		return "s3:" + strings.TrimSuffix(s.Endpoint, "/") + "/" + bucket
	}
	return "b2:" + bucket + ":/"
}

// Env returns the credentials as restic reads them.
func (s Storage) Env() []string {
	if s.Kind == "s3" {
		return []string{"AWS_ACCESS_KEY_ID=" + s.KeyID, "AWS_SECRET_ACCESS_KEY=" + s.Key}
	}
	return []string{"B2_ACCOUNT_ID=" + s.KeyID, "B2_ACCOUNT_KEY=" + s.Key}
}

// Describe says where backups go, without credentials.
func (s Storage) Describe() string {
	if s.Kind == "s3" {
		return "s3 at " + s.Endpoint + ", buckets " + s.BucketPrefix + "-*"
	}
	return "Backblaze B2, buckets " + s.BucketPrefix + "-*"
}

// Email is the mail server alerts go through.
type Email struct {
	Host     string
	Port     int
	User     string
	Password string
	From     string
	To       []string
}

// LoadEmail reads the email integration.
func LoadEmail(store *secrets.Store) (*Email, error) {
	v, err := Get(store, EmailName)
	if err != nil {
		return nil, err
	}
	port, _ := strconv.Atoi(v["smtp_port"])
	e := &Email{Host: v["smtp_host"], Port: port, User: v["smtp_user"], Password: v["smtp_password"], From: v["from"]}
	for _, addr := range strings.Split(v["to"], ",") {
		if a := strings.TrimSpace(addr); a != "" {
			e.To = append(e.To, a)
		}
	}
	return e, nil
}

// Cloudflare is the DNS API token.
type Cloudflare struct {
	Token string
}

// LoadCloudflare reads the cloudflare integration.
func LoadCloudflare(store *secrets.Store) (*Cloudflare, error) {
	v, err := Get(store, CloudflareName)
	if err != nil {
		return nil, err
	}
	return &Cloudflare{Token: v["token"]}, nil
}
