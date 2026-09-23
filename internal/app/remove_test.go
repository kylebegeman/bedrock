package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/bedrock/internal/integration"
	"github.com/kylebegeman/bedrock/internal/state"
)

func seedApp(t *testing.T, store *state.Store, app string) {
	t.Helper()
	manifest := `{"app":"` + app + `","workloads":{"web":{"kind":"web","image":"x","port":80,"routes":[{"host":"` + app + `.example.com"}]}}}`
	rev := state.Revision{App: app, ID: "r1", Status: state.RevisionActive, Manifest: json.RawMessage(manifest), CreatedAt: time.Now().UTC()}
	if err := store.SaveRevision(context.Background(), rev); err != nil {
		t.Fatal(err)
	}
}

func TestRemovingAPreviewWithItsDataForgetsItsSecrets(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seedApp(t, store, "site")
	seedApp(t, store, "site-pr-nav")
	r := Remove{Store: store, Secrets: newSecrets(t), StateDir: t.TempDir()}
	forget := func(in RemoveInput) string {
		t.Helper()
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := r.Plan(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		return plan.Steps[len(plan.Steps)-1].Change
	}
	if c := forget(RemoveInput{App: "site-pr-nav", Data: true}); !strings.Contains(c, "the secrets it was given as a preview") {
		t.Fatalf("a preview removed with its data keeps its secrets: %q", c)
	}
	if c := forget(RemoveInput{App: "site-pr-nav"}); strings.Contains(c, "secrets") {
		t.Fatalf("a preview kept for its data lost its secrets: %q", c)
	}
	if c := forget(RemoveInput{App: "site", Data: true}); strings.Contains(c, "secrets") {
		t.Fatalf("an app removed with its data lost its secrets unasked: %q", c)
	}
	if c := forget(RemoveInput{App: "site", Secrets: true}); !strings.Contains(c, ", and its secrets") {
		t.Fatalf("--secrets was not honoured: %q", c)
	}
}

func TestForgettingAnAppClearsWhatTheMachineKeptForIt(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	seedApp(t, store, "hello")
	if err := store.SaveGitHook(ctx, state.GitHook{App: "hello", Repo: "r", Branch: "main", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	sec := newSecrets(t)
	hook, key, storage := "hook", "key", "storage"
	if _, err := sec.SetAll(integration.App, map[string]*string{integration.WebhookSecretName("hello"): &hook, integration.DeployKeyName("hello"): &key, "STORAGE_KEY": &storage}); err != nil {
		t.Fatal(err)
	}
	if _, err := sec.Set("hello", "API_KEY", "v"); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	for _, d := range []string{"backups/hello", "restore/hello", "drills/hello", "builds/hello", "backups/other"} {
		if err := os.MkdirAll(filepath.Join(stateDir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var out strings.Builder
	if err := forgetApp(ctx, store, sec, stateDir, "hello", false, &out); err != nil {
		t.Fatal(err)
	}
	if revs, _ := store.Revisions(ctx, "hello"); len(revs) != 0 {
		t.Fatalf("revisions survived: %+v", revs)
	}
	if h, _ := store.GitHookFor(ctx, "hello"); h != nil {
		t.Fatalf("the webhook survived: %+v", h)
	}
	for _, d := range []string{"backups/hello", "restore/hello", "drills/hello", "builds/hello"} {
		if _, err := os.Stat(filepath.Join(stateDir, d)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived: %v", d, err)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, "backups/other")); err != nil {
		t.Fatalf("another app's directory went: %v", err)
	}
	own, _, err := sec.LoadCurrent(integration.App)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := own[integration.WebhookSecretName("hello")]; ok {
		t.Fatal("the webhook secret survived")
	}
	if own["STORAGE_KEY"] != "storage" {
		t.Fatal("an integration credential went with the app")
	}
	if v, _, _ := sec.LoadCurrent("hello"); v["API_KEY"] != "v" {
		t.Fatalf("the app's secrets went unasked: %v", v)
	}
	if !strings.Contains(out.String(), "webhook secret and deploy key") {
		t.Fatalf("output: %q", out.String())
	}
	if err := forgetApp(ctx, store, sec, stateDir, "hello", true, &out); err != nil {
		t.Fatal(err)
	}
	if current, _ := sec.Current("hello"); current != 0 {
		t.Fatalf("the app's secrets survived --secrets: version %d", current)
	}
}
