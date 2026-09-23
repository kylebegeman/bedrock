package gitdeploy

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Request is what an SSH client asked the bedrock user for.
type Request struct {
	// Kind is receive (git push), upload (git fetch) or tarball (bedrock
	// deploy --to).
	Kind string
	App  string
}

// Request kinds.
const (
	Receive = "receive"
	Upload  = "upload"
	Tarball = "tarball"
)

// ParseCommand reads SSH_ORIGINAL_COMMAND: what git or bedrock on the other
// end asked to run. Anything else is refused.
func ParseCommand(cmd string) (Request, error) {
	fields := strings.Fields(strings.TrimSpace(cmd))
	var kind, arg string
	switch {
	case len(fields) == 2 && (fields[0] == "git-receive-pack" || fields[0] == "git-upload-pack"):
		kind, arg = strings.TrimPrefix(fields[0], "git-"), fields[1]
	case len(fields) == 3 && fields[0] == "git" && (fields[1] == "receive-pack" || fields[1] == "upload-pack"):
		kind, arg = fields[1], fields[2]
	case len(fields) == 3 && fields[0] == "bedrock" && fields[1] == "receive":
		kind, arg = Tarball, fields[2]
	case len(fields) == 0:
		return Request{}, errors.New("this key deploys apps with git push or bedrock deploy --to; it opens no shell")
	default:
		return Request{}, fmt.Errorf("this key only deploys apps; %q isn't something it runs", fields[0])
	}
	switch kind {
	case "receive-pack":
		kind = Receive
	case "upload-pack":
		kind = Upload
	}
	arg = strings.Trim(arg, `'"`)
	arg = strings.TrimPrefix(arg, "~/")
	arg = strings.TrimPrefix(arg, "/")
	arg = strings.TrimSuffix(arg, ".git")
	if !ValidApp(arg) {
		return Request{}, fmt.Errorf("%q isn't an app's name; push to bedrock@<machine>:<app>.git", arg)
	}
	return Request{Kind: kind, App: arg}, nil
}

// Allowed checks a request against the apps a key may deploy.
func (r Request) Allowed(apps []string) error {
	for _, a := range apps {
		if a == r.App {
			return nil
		}
	}
	return fmt.Errorf("this key deploys %s, not %s; on the machine, bedrock git allow %s <key> adds it", strings.Join(apps, ", "), r.App, r.App)
}

// RepoDir is where an app's repository lives.
func RepoDir(app string) string { return filepath.Join(Home, app+".git") }

// DefaultBranch is the branch whose pushes deploy.
const DefaultBranch = "main"

// EnsureRepo makes an app's bare repository on first push and keeps its
// hook pointing at this bedrock.
func EnsureRepo(app string) (string, error) {
	dir := RepoDir(app)
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); errors.Is(err, os.ErrNotExist) {
		if out, err := exec.Command("git", "init", "--bare", "--quiet", "--initial-branch="+DefaultBranch, dir).CombinedOutput(); err != nil {
			return "", fmt.Errorf("make the repository for %s: %s", app, strings.TrimSpace(string(out)))
		}
		for _, kv := range [][2]string{{"receive.fsckObjects", "true"}, {"bedrock.branch", DefaultBranch}} {
			if out, err := exec.Command("git", "-C", dir, "config", kv[0], kv[1]).CombinedOutput(); err != nil {
				return "", fmt.Errorf("configure the repository for %s: %s", app, strings.TrimSpace(string(out)))
			}
		}
	}
	hook := "#!/bin/sh\n# Written by bedrock: a push to the deploy branch deploys, and a failed deploy refuses the push.\nexec " + Binary + " git hook " + app + "\n"
	path := filepath.Join(dir, "hooks", "pre-receive")
	if current, _ := os.ReadFile(path); string(current) != hook {
		// Replaced whole: git runs whatever is there on the next push.
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(hook), 0o755); err != nil {
			return "", err
		}
		if err := os.Rename(tmp, path); err != nil {
			_ = os.Remove(tmp)
			return "", err
		}
	}
	return dir, nil
}

// Branch is the branch whose pushes deploy an app.
func Branch(repoDir string) string {
	out, err := exec.Command("git", "-C", repoDir, "config", "--get", "bedrock.branch").Output()
	if b := strings.TrimSpace(string(out)); err == nil && b != "" {
		return b
	}
	return DefaultBranch
}

// SetBranch changes which branch deploys.
func SetBranch(repoDir, branch string) error {
	if out, err := exec.Command("git", "-C", repoDir, "config", "bedrock.branch", branch).CombinedOutput(); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}
