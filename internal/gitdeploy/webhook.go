package gitdeploy

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kylebegeman/bedrock/internal/app"
	"github.com/kylebegeman/bedrock/internal/integration"
	"github.com/kylebegeman/bedrock/internal/kernel"
	"github.com/kylebegeman/bedrock/internal/secrets"
	"github.com/kylebegeman/bedrock/internal/state"
)

// HookPath is where webhooks arrive, on the app's own first host.
const HookPath = "/_bedrock/hook"

// secretName names an app's webhook secret and deploy key in bedrock's own
// secrets. App names have no underscores, so the mapping is one to one.
func secretName(kind, app string) string {
	return kind + "_" + strings.ToUpper(strings.ReplaceAll(app, "-", "_"))
}

// ValidRepo checks a repository URL bedrock can fetch.
func ValidRepo(repo string) error {
	switch {
	case strings.HasPrefix(repo, "https://"), strings.HasPrefix(repo, "http://"), strings.HasPrefix(repo, "ssh://"), strings.HasPrefix(repo, "file:///"):
		return nil
	case strings.HasPrefix(repo, "git@") && strings.Contains(repo, ":"):
		return nil
	}
	return fmt.Errorf("%q isn't a repository URL bedrock can fetch (https://, git@host:owner/repo.git, ssh:// or file:///)", repo)
}

func usesSSH(repo string) bool {
	return strings.HasPrefix(repo, "ssh://") || strings.HasPrefix(repo, "git@")
}

// Webhook is what a person needs to set a webhook up on GitHub.
type Webhook struct {
	URL    string
	Secret string
	// DeployKey is the public key to add as a read-only deploy key, for
	// a repository fetched over SSH.
	DeployKey string
}

// Configure sets an app's webhook up: a secret for GitHub to sign with,
// and, for a repository reached over SSH, a deploy key of its own. Both
// live in bedrock's own secrets; the caller shows them once.
func Configure(ctx context.Context, store *state.Store, sec *secrets.Store, appName, repo, branch, host string) (*Webhook, error) {
	if err := ValidRepo(repo); err != nil {
		return nil, err
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	w := &Webhook{URL: "https://" + host + HookPath, Secret: hex.EncodeToString(b[:])}
	secret := w.Secret
	changes := map[string]*string{secretName("WEBHOOK_SECRET", appName): &secret, secretName("DEPLOY_KEY", appName): nil}
	if usesSSH(repo) {
		private, public, err := newDeployKey(appName)
		if err != nil {
			return nil, err
		}
		changes[secretName("DEPLOY_KEY", appName)] = &private
		w.DeployKey = public
	}
	if _, err := sec.SetAll(integration.App, changes); err != nil {
		return nil, err
	}
	if err := store.SaveGitHook(ctx, state.GitHook{App: appName, Repo: repo, Branch: branch, CreatedAt: time.Now().UTC()}); err != nil {
		return nil, err
	}
	return w, nil
}

// Disable forgets an app's webhook and its secrets.
func Disable(ctx context.Context, store *state.Store, sec *secrets.Store, appName string) error {
	if err := store.RemoveGitHook(ctx, appName); err != nil {
		return err
	}
	_, err := sec.SetAll(integration.App, map[string]*string{secretName("WEBHOOK_SECRET", appName): nil, secretName("DEPLOY_KEY", appName): nil})
	return err
}

// newDeployKey makes an ed25519 key pair with ssh-keygen and returns the
// private key's text and the public key's line.
func newDeployKey(appName string) (string, string, error) {
	dir, err := os.MkdirTemp("", "bedrock-key-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(dir)
	host, _ := os.Hostname()
	path := filepath.Join(dir, "key")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "bedrock deploy key for "+appName+" on "+host, "-f", path).CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("ssh-keygen: %s", strings.TrimSpace(string(out)))
	}
	private, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	public, err := os.ReadFile(path + ".pub")
	if err != nil {
		return "", "", err
	}
	return string(private), strings.TrimSpace(string(public)), nil
}

// VerifySignature checks GitHub's X-Hub-Signature-256 header against the
// body and the secret.
func VerifySignature(secret string, body []byte, header string) bool {
	given, ok := strings.CutPrefix(header, "sha256=")
	if !ok || secret == "" {
		return false
	}
	want, err := hex.DecodeString(given)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}

// Receiver answers webhooks and deploys what they announce, one deploy per
// app at a time; pushes that arrive meanwhile collapse into the newest.
type Receiver struct {
	Store   *state.Store
	Secrets *secrets.Store
	Deploy  Deployer
	// Notify tells a person when a deploy from a webhook fails.
	Notify func(ctx context.Context, subject, body string)
	Log    func(format string, args ...any)
	// SourcesDir holds the fetched mirrors; BuildsDir the source trees.
	SourcesDir string
	BuildsDir  string

	mu      sync.Mutex
	running map[string]bool
	pending map[string]bool
}

type pushEvent struct {
	Ref     string `json:"ref"`
	After   string `json:"after"`
	Deleted bool   `json:"deleted"`
}

// ServeHTTP implements http.Handler.
func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost || strings.TrimSuffix(req.URL.Path, "/") != HookPath {
		http.NotFound(w, req)
		return
	}
	ctx := req.Context()
	host := req.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	appName, err := r.appFor(ctx, host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hook, err := r.Store.GitHookFor(ctx, appName)
	if err != nil || hook == nil || appName == "" {
		http.NotFound(w, req)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 5<<20))
	if err != nil {
		http.Error(w, "can't read the body", http.StatusBadRequest)
		return
	}
	values, _, err := r.Secrets.LoadCurrent(integration.App)
	if err != nil {
		http.Error(w, "secrets unavailable", http.StatusInternalServerError)
		return
	}
	if !VerifySignature(values[secretName("WEBHOOK_SECRET", appName)], body, req.Header.Get("X-Hub-Signature-256")) {
		r.Log("webhook for %s: bad signature from %s", appName, req.Header.Get("X-Forwarded-For"))
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	switch req.Header.Get("X-GitHub-Event") {
	case "ping":
		fmt.Fprintf(w, "bedrock hears %s's pushes to %s\n", appName, hook.Branch)
		return
	case "push":
	default:
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintln(w, "ignored: only pushes deploy")
		return
	}
	var ev pushEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "bad push payload", http.StatusBadRequest)
		return
	}
	if ev.Deleted || ev.Ref != "refs/heads/"+hook.Branch {
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, "ignored: %s isn't %s\n", ev.Ref, hook.Branch)
		return
	}
	r.enqueue(appName)
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, "deploying %s of %s\n", short(ev.After), appName)
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// appFor finds the app a host belongs to.
func (r *Receiver) appFor(ctx context.Context, host string) (string, error) {
	routes, err := app.Routes(ctx, r.Store)
	if err != nil {
		return "", err
	}
	for _, rt := range routes {
		if rt.Host == host {
			return rt.App, nil
		}
	}
	return "", nil
}

// enqueue starts a deploy for an app, or marks one as wanted after the
// deploy that is running: the branch's newest commit is what it deploys.
func (r *Receiver) enqueue(appName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running == nil {
		r.running, r.pending = map[string]bool{}, map[string]bool{}
	}
	if r.running[appName] {
		r.pending[appName] = true
		return
	}
	r.running[appName] = true
	go r.work(appName)
}

func (r *Receiver) work(appName string) {
	for {
		r.deployOnce(appName)
		r.mu.Lock()
		if r.pending[appName] {
			delete(r.pending, appName)
			r.mu.Unlock()
			continue
		}
		delete(r.running, appName)
		r.mu.Unlock()
		return
	}
}

// Wait blocks until no webhook deploy is running, for tests and shutdown.
func (r *Receiver) Wait(ctx context.Context) {
	for {
		r.mu.Lock()
		busy := len(r.running) > 0
		r.mu.Unlock()
		if !busy {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (r *Receiver) deployOnce(appName string) {
	ctx := context.Background()
	hook, err := r.Store.GitHookFor(ctx, appName)
	if err != nil || hook == nil {
		return
	}
	commit, dir, err := r.fetch(ctx, *hook)
	var result string
	if err != nil {
		result = "fetch failed: " + err.Error()
	} else {
		var log strings.Builder
		ok, derr := Deploy(ctx, r.Deploy, dir, short(commit), &log)
		switch {
		case derr != nil:
			result = "failed: " + derr.Error()
		case !ok:
			result = "failed: " + lastFailure(log.String())
		default:
			result = "deployed"
		}
		PruneBuilds(r.BuildsDir, appName, 3)
	}
	_ = r.Store.RecordGitHook(ctx, appName, short(commit), result, time.Now().UTC())
	r.Log("webhook deploy of %s %s: %s", appName, short(commit), result)
	if result != "deployed" && r.Notify != nil {
		r.Notify(ctx, appName+": the deploy from "+hook.Repo+" failed",
			fmt.Sprintf("A push to %s of %s asked for a deploy, and it %s.\n\nThe running revision stays. bedrock history on the machine has the receipt.\n", hook.Branch, hook.Repo, result))
	}
}

// lastFailure picks the failing step's line out of a rendered deploy.
func lastFailure(log string) string {
	lines := strings.Split(strings.TrimSpace(log), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "fail ") {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), "fail"))
		}
	}
	return lines[len(lines)-1]
}

// fetch brings the branch's newest commit into the app's mirror and
// writes its tree out.
func (r *Receiver) fetch(ctx context.Context, h state.GitHook) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	mirror := filepath.Join(r.SourcesDir, h.App+".git")
	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(r.SourcesDir, 0o700); err != nil {
			return "", "", err
		}
		if out, err := exec.CommandContext(ctx, "git", "init", "--bare", "--quiet", mirror).CombinedOutput(); err != nil {
			return "", "", fmt.Errorf("%s", strings.TrimSpace(string(out)))
		}
	}
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if usesSSH(h.Repo) {
		values, _, err := r.Secrets.LoadCurrent(integration.App)
		if err != nil {
			return "", "", err
		}
		key := values[secretName("DEPLOY_KEY", h.App)]
		if key == "" {
			return "", "", errors.New("no deploy key; run bedrock git webhook again")
		}
		keyFile := filepath.Join(r.SourcesDir, "."+h.App+".key")
		if err := os.WriteFile(keyFile, []byte(key), 0o600); err != nil {
			return "", "", err
		}
		defer os.Remove(keyFile)
		env = append(env, "GIT_SSH_COMMAND=ssh -i "+keyFile+" -o IdentitiesOnly=yes -o IdentityAgent=none -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile="+filepath.Join(r.SourcesDir, "known_hosts"))
	}
	ref := "refs/heads/" + h.Branch
	fetch := exec.CommandContext(ctx, "git", "-C", mirror, "fetch", "--quiet", "--force", "--depth", "1", h.Repo, ref+":"+ref)
	fetch.Env = env
	if out, err := fetch.CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("git fetch %s %s: %s", h.Repo, h.Branch, strings.TrimSpace(string(out)))
	}
	out, err := exec.CommandContext(ctx, "git", "-C", mirror, "rev-parse", ref).Output()
	if err != nil {
		return "", "", err
	}
	commit := strings.TrimSpace(string(out))
	dir, err := NewBuildDir(r.BuildsDir, h.App, short(commit))
	if err != nil {
		return commit, "", err
	}
	if err := ExportCommit(ctx, mirror, commit, dir); err != nil {
		return commit, "", err
	}
	if _, err := CheckSource(dir, h.App); err != nil {
		return commit, "", err
	}
	return commit, dir, nil
}

// InProcess is a Deployer for the daemon itself, which owns the kernel.
func InProcess(run func(ctx context.Context, kind string, input json.RawMessage, emit func(kernel.Event)) (*kernel.Receipt, error)) Deployer {
	return func(ctx context.Context, in app.DeployInput, emit func(kernel.Event)) (*kernel.Receipt, error) {
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		return run(ctx, app.DeployKind, raw, emit)
	}
}
