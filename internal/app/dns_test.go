package app

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/bedrock/internal/cloudflare/cloudflaretest"
	"github.com/kylebegeman/bedrock/internal/integration"
	"github.com/kylebegeman/bedrock/internal/state"
)

// The production machine after loom was removed by hand: its records
// still point here, and bedrock dns audit says nothing answers for them.
func TestDroppingARecordNoRouteHereKeeps(t *testing.T) {
	ctx := context.Background()
	const here = "187.127.249.208"
	fake := cloudflaretest.New("t0ken", "begam.in")
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	fake.Seed(cloudflaretest.Record{Type: "A", Name: "loom.begam.in", Content: here})
	fake.Seed(cloudflaretest.Record{Type: "A", Name: "runner.loom.begam.in", Content: here})
	fake.Seed(cloudflaretest.Record{Type: "A", Name: "*.loom.begam.in", Content: here})
	fake.Seed(cloudflaretest.Record{Type: "A", Name: "*.pr.begam.in", Content: here})
	fake.Seed(cloudflaretest.Record{Type: "A", Name: "hello.begam.in", Content: here})
	fake.Seed(cloudflaretest.Record{Type: "A", Name: "old.begam.in", Content: here})
	fake.Seed(cloudflaretest.Record{Type: "A", Name: "site.begam.in", Content: "198.51.100.20", Comment: "bedrock: site on " + here})

	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	seedManifest(t, store, "hello", `{"app":"hello","workloads":{"web":{"kind":"web","image":"x","port":80,"routes":[{"host":"hello.begam.in"}]}}}`)
	// A previous revision routes old.begam.in: a rollback would want it.
	old := state.Revision{App: "hello", ID: "r0", Status: state.RevisionPrevious, CreatedAt: time.Now().Add(-time.Hour).UTC(),
		Manifest: json.RawMessage(`{"app":"hello","workloads":{"web":{"kind":"web","image":"x","port":80,"routes":[{"host":"old.begam.in"},{"host":"a.pr.begam.in"}]}}}`)}
	if err := store.SaveRevision(ctx, old); err != nil {
		t.Fatal(err)
	}
	sec := newSecrets(t)
	d := Drop{Store: store, Secrets: sec, Addresses: func(context.Context) []string { return []string{here} }}
	plan := func(host string) error {
		_, err := d.Plan(ctx, json.RawMessage(`{"host":"`+host+`"}`))
		return err
	}
	if err := plan("loom.begam.in"); err == nil || !strings.Contains(err.Error(), "needs the cloudflare integration") {
		t.Fatalf("without the integration: %v", err)
	}
	if _, _, err := integration.Set(sec, integration.CloudflareName, map[string]string{"token": "t0ken", "api": srv.URL + "/client/v4"}); err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]string{
		"hello.begam.in":  "hello routes hello.begam.in on this machine",
		"old.begam.in":    "hello routes old.begam.in on this machine",
		"*.pr.begam.in":   "*.pr.begam.in covers a.pr.begam.in, which hello routes",
		"site.begam.in":   "names 198.51.100.20, not this machine",
		"gone.begam.in":   "has no A or AAAA record",
		"not a host":      "isn't a hostname",
		"*.*.begam.in":    "isn't a hostname",
		"loom.example.io": "no zone",
	} {
		if err := plan(host); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", host, err, want)
		}
	}

	p, err := d.Plan(ctx, json.RawMessage(`{"host":"Loom.Begam.in."}`))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "plan-dns-drop", planText(p))
	var out strings.Builder
	for _, st := range p.Steps {
		if err := st.Apply(ctx, &out); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(out.String(), "loom.begam.in's A 187.127.249.208 record deleted") {
		t.Fatalf("output: %q", out.String())
	}
	var left []string
	for _, r := range fake.Records() {
		left = append(left, r.Name)
	}
	// The wildcard over it and the name below it stay: each goes by its
	// own name only.
	if got := strings.Join(left, " "); got != "*.loom.begam.in *.pr.begam.in hello.begam.in old.begam.in runner.loom.begam.in site.begam.in" {
		t.Fatalf("left: %s", got)
	}
	if err := plan("*.loom.begam.in"); err != nil {
		t.Fatalf("a wildcard no route falls under, named: %v", err)
	}
}
