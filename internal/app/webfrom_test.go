package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/bedrock/internal/manifest"
	"github.com/kylebegeman/bedrock/internal/state"
)

func answers(yes bool) CloudflareOnly { return func() (bool, error) { return yes, nil } }

// On a machine that answers only Cloudflare, a route with its own record
// or a manual one would be unreachable the moment it deployed, so neither
// a deploy nor a rollback to such a revision gets a plan.
func TestAMachineThatAnswersOnlyCloudflareRefusesRoutesThatBypassIt(t *testing.T) {
	ctx := context.Background()
	m, dir := loadCore(t)
	d := Deploy{Secrets: newSecrets(t), StateDir: t.TempDir(), CloudflareOnly: answers(true)}
	from := &buildFrom{source: dir, commit: "0123456789ab"}

	_, err := d.rollout(ctx, m, "20260922-120000", from)
	if err == nil || !strings.Contains(err.Error(), "only from Cloudflare") || !strings.Contains(err.Error(), "core.example.com (dns: manual)") {
		t.Fatalf("manual routes: %v", err)
	}
	SetRouteDNS(m, manifest.DNSDirect)
	if _, err := d.rollout(ctx, m, "20260922-120000", from); err == nil || !strings.Contains(err.Error(), "runner.example.com (dns: direct)") {
		t.Fatalf("direct routes: %v", err)
	}
	SetRouteDNS(m, manifest.DNSProxied)
	if _, err := d.rollout(ctx, m, "20260922-120000", from); err != nil {
		t.Fatalf("proxied routes are what such a machine takes: %v", err)
	}
	// A machine open to anyone takes any route, and one whose profile
	// cannot be read takes none.
	SetRouteDNS(m, manifest.DNSDirect)
	d.CloudflareOnly = answers(false)
	if _, err := d.rollout(ctx, m, "20260922-120000", from); err != nil {
		t.Fatal(err)
	}
	d.CloudflareOnly = func() (bool, error) { return false, errors.New("profile unreadable") }
	if _, err := d.rollout(ctx, m, "20260922-120000", from); err == nil || !strings.Contains(err.Error(), "profile unreadable") {
		t.Fatalf("an unreadable profile must stop the deploy: %v", err)
	}

	store := goldenStore(t)
	raw, _ := json.Marshal(m)
	if err := store.SaveRevision(ctx, state.Revision{App: "loom", ID: "20260918-120000", Status: state.RevisionPrevious, Manifest: raw, Images: map[string]string{}, Containers: map[string]string{}, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	rollback := Rollback{Deploy: Deploy{Store: store, StateDir: t.TempDir(), CloudflareOnly: answers(true)}}
	if _, err := rollback.Plan(ctx, json.RawMessage(`{"app":"loom"}`)); err == nil || !strings.Contains(err.Error(), "only from Cloudflare") {
		t.Fatalf("rollback to direct routes: %v", err)
	}
}

func TestPointingANameStraightAtAMachineThatAnswersOnlyCloudflareIsRefused(t *testing.T) {
	ctx := context.Background()
	m, _ := loadCore(t)
	SetRouteDNS(m, manifest.DNSProxied)
	raw, _ := json.Marshal(m)
	store := goldenStore(t)
	if err := store.SaveRevision(ctx, state.Revision{App: "loom", ID: "r1", Status: state.RevisionActive, Manifest: raw, Images: map[string]string{}, Containers: map[string]string{}, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	p := Point{Store: store, Secrets: newSecrets(t), CloudflareOnly: answers(true)}
	// The route asks for the proxy; --proxied is not needed.
	if _, err := p.Plan(ctx, json.RawMessage(`{"host":"core.example.com"}`)); err == nil || strings.Contains(err.Error(), "only from Cloudflare") {
		t.Fatalf("a proxied route must pass the check (and stop only for the missing integration): %v", err)
	}
	SetRouteDNS(m, manifest.DNSDirect)
	raw, _ = json.Marshal(m)
	if err := store.SaveRevision(ctx, state.Revision{App: "loom", ID: "r1", Status: state.RevisionActive, Manifest: raw, Images: map[string]string{}, Containers: map[string]string{}, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Plan(ctx, json.RawMessage(`{"host":"core.example.com"}`)); err == nil || !strings.Contains(err.Error(), "point core.example.com with --proxied") {
		t.Fatalf("a direct record must be refused: %v", err)
	}
	if _, err := p.Plan(ctx, json.RawMessage(`{"host":"core.example.com","proxied":true}`)); err == nil || strings.Contains(err.Error(), "only from Cloudflare") {
		t.Fatalf("--proxied must pass the check: %v", err)
	}
}
