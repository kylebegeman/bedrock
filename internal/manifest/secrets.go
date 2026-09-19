package manifest

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// SecretFormat is how a generated secret is made.
type SecretFormat struct {
	Kind  string // hex, base64, base64url or value
	Bytes int
	Value string
}

// ParseSecretFormat reads hex:N, base64:N, base64url:N or value:TEXT.
func ParseSecretFormat(s string) (SecretFormat, error) {
	kind, arg, ok := strings.Cut(s, ":")
	if !ok {
		return SecretFormat{}, fmt.Errorf("%q must be hex:N, base64:N, base64url:N or value:TEXT", s)
	}
	switch kind {
	case "value":
		if arg == "" || strings.ContainsAny(arg, "\r\n\x00") {
			return SecretFormat{}, errors.New("value: needs a single line of text")
		}
		return SecretFormat{Kind: kind, Value: arg}, nil
	case "hex", "base64", "base64url":
		n, err := strconv.Atoi(arg)
		if err != nil || n < 8 || n > 256 {
			return SecretFormat{}, fmt.Errorf("%s: wants 8 to 256 random bytes, not %q", kind, arg)
		}
		return SecretFormat{Kind: kind, Bytes: n}, nil
	}
	return SecretFormat{}, fmt.Errorf("%q isn't hex, base64, base64url or value", kind)
}

// Make produces a value in the format.
func (f SecretFormat) Make() (string, error) {
	if f.Kind == "value" {
		return f.Value, nil
	}
	b := make([]byte, f.Bytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	switch f.Kind {
	case "hex":
		return hex.EncodeToString(b), nil
	case "base64":
		return base64.StdEncoding.EncodeToString(b), nil
	default:
		return base64.RawURLEncoding.EncodeToString(b), nil
	}
}

var placeholder = regexp.MustCompile(`\{([^{}]*)\}`)

// checkTemplate checks a derive template's placeholders.
func checkTemplate(tmpl string) error {
	if strings.ContainsAny(tmpl, "\r\n\x00") {
		return errors.New("must be one line")
	}
	for _, m := range placeholder.FindAllStringSubmatch(tmpl, -1) {
		ref := m[1]
		switch {
		case ref == "postgres" || ref == "objects":
		case strings.HasPrefix(ref, "sha256:") && envPattern.MatchString(strings.TrimPrefix(ref, "sha256:")):
		case envPattern.MatchString(ref):
		default:
			return fmt.Errorf("{%s} isn't a secret's name, {sha256:NAME}, {postgres} or {objects}", ref)
		}
	}
	rest := placeholder.ReplaceAllString(tmpl, "")
	if strings.ContainsAny(rest, "{}") {
		return errors.New("has an unmatched { or }")
	}
	return nil
}

// Derive computes derived secrets from the values known so far and the
// data services' hosts. Derived secrets may use each other; a name that
// never becomes known is an error.
func Derive(derive map[string]string, values map[string]string, hosts map[string]string) (map[string]string, error) {
	out := map[string]string{}
	names := make([]string, 0, len(derive))
	for n := range derive {
		names = append(names, n)
	}
	sort.Strings(names)
	known := func(name string) (string, bool) {
		if v, ok := out[name]; ok {
			return v, true
		}
		v, ok := values[name]
		return v, ok
	}
	pending := names
	for pass := 0; pass <= len(names) && len(pending) > 0; pass++ {
		var next []string
		for _, name := range pending {
			missing := ""
			value := placeholder.ReplaceAllStringFunc(derive[name], func(p string) string {
				ref := p[1 : len(p)-1]
				if h, ok := hosts[ref]; ok && (ref == "postgres" || ref == "objects") {
					return h
				}
				if src, ok := strings.CutPrefix(ref, "sha256:"); ok {
					v, found := known(src)
					if !found {
						missing = src
						return ""
					}
					sum := sha256.Sum256([]byte(v))
					return hex.EncodeToString(sum[:])
				}
				v, found := known(ref)
				if !found {
					missing = ref
				}
				return v
			})
			if missing != "" {
				next = append(next, name)
				continue
			}
			out[name] = value
		}
		pending = next
	}
	if len(pending) > 0 {
		return nil, fmt.Errorf("%s can't be derived: a secret it uses doesn't exist", strings.Join(pending, ", "))
	}
	return out, nil
}
