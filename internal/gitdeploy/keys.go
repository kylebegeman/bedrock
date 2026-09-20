// Package gitdeploy is how code reaches a machine without anything in
// between: git push to the machine, a tarball over one SSH session, or a
// GitHub webhook. Pushes arrive as the bedrock user, whose every key is
// bound to the apps it may deploy; the deploy itself runs in the daemon
// like any other, and its steps stream back to the person pushing.
package gitdeploy

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Where pushes land.
const (
	// User receives pushes over SSH; it has no password and every one of
	// its keys runs bedrock and nothing else.
	User = "bedrock"
	// Home holds the repositories and the build directories.
	Home = "/var/lib/bedrock-git"
	// AuthorizedKeys is the bedrock user's key file, which bedrock writes.
	AuthorizedKeys = Home + "/.ssh/authorized_keys"
	// BuildsDir holds the source trees deploys are made from.
	BuildsDir = Home + "/builds"
	// Binary is what each key runs.
	Binary = "/usr/local/bin/bedrock"
)

// Key is one SSH public key and the apps it may deploy.
type Key struct {
	Type    string   `json:"type"`
	Blob    string   `json:"-"`
	Comment string   `json:"comment,omitempty"`
	Apps    []string `json:"apps"`
}

var keyTypes = map[string]bool{
	"ssh-ed25519": true, "ssh-rsa": true,
	"ecdsa-sha2-nistp256": true, "ecdsa-sha2-nistp384": true, "ecdsa-sha2-nistp521": true,
	"sk-ssh-ed25519@openssh.com": true, "sk-ecdsa-sha2-nistp256@openssh.com": true,
}

var appPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// ValidApp reports whether a name can be an app's.
func ValidApp(app string) bool { return appPattern.MatchString(app) }

// ParseKey reads a public key line, such as the contents of id_ed25519.pub.
func ParseKey(text string) (Key, error) {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) < 2 {
		return Key{}, errors.New("that isn't an SSH public key; give the one line of a .pub file")
	}
	k := Key{Type: fields[0], Blob: fields[1]}
	if !keyTypes[k.Type] {
		return Key{}, fmt.Errorf("%q isn't an SSH public key type bedrock accepts", k.Type)
	}
	raw, err := base64.StdEncoding.DecodeString(k.Blob)
	if err != nil {
		return Key{}, errors.New("the key's body isn't base64")
	}
	if len(raw) < 4 {
		return Key{}, errors.New("the key is too short")
	}
	n := binary.BigEndian.Uint32(raw[:4])
	if int(n)+4 > len(raw) || string(raw[4:4+n]) != k.Type {
		return Key{}, errors.New("the key's body doesn't match its type")
	}
	if len(fields) > 2 {
		k.Comment = cleanComment(strings.Join(fields[2:], " "))
	}
	return k, nil
}

func cleanComment(c string) string {
	c = strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 32 {
			return -1
		}
		return r
	}, c)
	if len(c) > 80 {
		c = c[:80]
	}
	return c
}

// Fingerprint is the key's SHA256 fingerprint, as ssh-keygen -l prints it.
func (k Key) Fingerprint() string {
	raw, _ := base64.StdEncoding.DecodeString(k.Blob)
	sum := sha256.Sum256(raw)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// Line is the key's authorized_keys line: the forced command that lets it
// deploy its apps and do nothing else.
func (k Key) Line() string {
	apps := append([]string{}, k.Apps...)
	sort.Strings(apps)
	line := fmt.Sprintf(`command="%s git serve %s",restrict %s %s`, Binary, strings.Join(apps, " "), k.Type, k.Blob)
	if k.Comment != "" {
		line += " " + k.Comment
	}
	return line
}

var managedLine = regexp.MustCompile(`^command="` + regexp.QuoteMeta(Binary) + ` git serve ([a-z0-9 -]+)",restrict (\S+) (\S+)(?: (.*))?$`)

// KeyFile is the bedrock user's authorized_keys: the keys bedrock manages,
// and any other line, kept as it was.
type KeyFile struct {
	Path   string
	Keys   []Key
	Others []string
}

// ReadKeys reads the key file; a missing one is empty.
func ReadKeys(path string) (*KeyFile, error) {
	kf := &KeyFile{Path: path}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return kf, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 16<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if m := managedLine.FindStringSubmatch(line); m != nil {
			kf.Keys = append(kf.Keys, Key{Type: m[2], Blob: m[3], Comment: m[4], Apps: strings.Fields(m[1])})
			continue
		}
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "# Written by bedrock") {
			kf.Others = append(kf.Others, line)
		}
	}
	return kf, sc.Err()
}

// Allow lets a key deploy an app, adding the key when it is new.
func (kf *KeyFile) Allow(k Key, app string) Key {
	for i := range kf.Keys {
		if kf.Keys[i].Blob == k.Blob {
			for _, a := range kf.Keys[i].Apps {
				if a == app {
					return kf.Keys[i]
				}
			}
			kf.Keys[i].Apps = append(kf.Keys[i].Apps, app)
			sort.Strings(kf.Keys[i].Apps)
			if k.Comment != "" {
				kf.Keys[i].Comment = k.Comment
			}
			return kf.Keys[i]
		}
	}
	k.Apps = []string{app}
	kf.Keys = append(kf.Keys, k)
	return k
}

// Deny takes an app from a key named by fingerprint or public key; a key
// left with no apps is removed. It reports whether anything changed.
func (kf *KeyFile) Deny(app, which string) bool {
	match := func(k Key) bool {
		if strings.HasPrefix(which, "SHA256:") {
			return k.Fingerprint() == which
		}
		if parsed, err := ParseKey(which); err == nil {
			return parsed.Blob == k.Blob
		}
		return false
	}
	changed := false
	var kept []Key
	for _, k := range kf.Keys {
		if match(k) {
			var apps []string
			for _, a := range k.Apps {
				if a == app {
					changed = true
					continue
				}
				apps = append(apps, a)
			}
			if len(apps) == 0 {
				continue
			}
			k.Apps = apps
		}
		kept = append(kept, k)
	}
	kf.Keys = kept
	return changed
}

// Write replaces the key file in one step, readable by its owner only.
// Ownership is the caller's (the bedrock user's, on a machine).
func (kf *KeyFile) Write() error {
	if err := os.MkdirAll(filepath.Dir(kf.Path), 0o700); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# Written by bedrock git allow. Each key deploys the apps its command names, and nothing else.\n")
	for _, line := range kf.Others {
		b.WriteString(line + "\n")
	}
	for _, k := range kf.Keys {
		b.WriteString(k.Line() + "\n")
	}
	tmp := kf.Path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, kf.Path)
}
