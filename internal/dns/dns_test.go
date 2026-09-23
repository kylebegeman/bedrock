package dns

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kylebegeman/bedrock/internal/cloudflare"
	"github.com/kylebegeman/bedrock/internal/cloudflare/cloudflaretest"
	"github.com/kylebegeman/bedrock/internal/manifest"
)

const here = "203.0.113.10"

func setup(t *testing.T) (*Manager, *cloudflaretest.Server) {
	t.Helper()
	fake := cloudflaretest.New("t0ken", "begam.in", "example.com")
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	cf := cloudflare.New("t0ken")
	cf.Base = srv.URL + "/client/v4"
	return &Manager{CF: cf, Hostname: "bedrock-lane", Addresses: []string{here, "2001:db8::1"}}, fake
}

func TestRecordsAreMadeKeptUpdatedAndRemoved(t *testing.T) {
	ctx := context.Background()
	m, fake := setup(t)
	out, err := m.Ensure(ctx, "begamin", "api.lane.begam.in", manifest.DNSDirect, false)
	if err != nil || out.Action != "made" {
		t.Fatalf("%+v %v", out, err)
	}
	recs := fake.Records()
	if len(recs) != 1 || recs[0].Content != here || recs[0].Proxied || recs[0].Comment != "bedrock: begamin on bedrock-lane" || recs[0].Type != "A" {
		t.Fatalf("%+v", recs)
	}
	if out, _ := m.Ensure(ctx, "begamin", "api.lane.begam.in", manifest.DNSDirect, false); out.Action != "kept" {
		t.Fatalf("second ensure: %+v", out)
	}
	if out, _ := m.Ensure(ctx, "begamin", "api.lane.begam.in", manifest.DNSProxied, false); out.Action != "updated" || !fake.Records()[0].Proxied {
		t.Fatalf("to proxied: %+v %+v", out, fake.Records())
	}
	// Someone's hand-made record for another host stays untouched.
	fake.Seed(cloudflaretest.Record{Type: "A", Name: "www.begam.in", Content: "203.0.113.9"})
	removed, err := m.Remove(ctx, "begamin", []string{"api.lane.begam.in", "www.begam.in"})
	if err != nil || strings.Join(removed, ",") != "api.lane.begam.in" {
		t.Fatalf("removed %v %v", removed, err)
	}
	if recs := fake.Records(); len(recs) != 1 || recs[0].Name != "www.begam.in" {
		t.Fatalf("left: %+v", recs)
	}
}

func TestARecordElsewhereIsNeverTakenSilently(t *testing.T) {
	ctx := context.Background()
	m, fake := setup(t)
	fake.Seed(cloudflaretest.Record{Type: "A", Name: "dragonwriter.begam.in", Content: "198.51.100.20", Comment: "bedrock: dragon-writer on braintreelabs"})
	_, err := m.Ensure(ctx, "dragon-writer", "dragonwriter.begam.in", manifest.DNSDirect, false)
	if !errors.Is(err, ErrElsewhere) || !strings.Contains(err.Error(), "198.51.100.20 (dragon-writer on braintreelabs)") || !strings.Contains(err.Error(), "bedrock dns point dragonwriter.begam.in") {
		t.Fatalf("%v", err)
	}
	// The other machine's record must not be removed by this machine.
	if removed, _ := m.Remove(ctx, "dragon-writer", []string{"dragonwriter.begam.in"}); len(removed) != 0 {
		t.Fatal("removed another machine's record")
	}
	out, err := m.Ensure(ctx, "dragon-writer", "dragonwriter.begam.in", manifest.DNSDirect, true)
	if err != nil || out.Action != "updated" {
		t.Fatalf("point: %+v %v", out, err)
	}
	if r := fake.Records()[0]; r.Content != here || r.Comment != "bedrock: dragon-writer on bedrock-lane" {
		t.Fatalf("%+v", r)
	}
}

func TestACNAMEBlocksUntilPointed(t *testing.T) {
	ctx := context.Background()
	m, fake := setup(t)
	fake.Seed(cloudflaretest.Record{Type: "CNAME", Name: "go.begam.in", Content: "old.example.net"})
	if _, err := m.Ensure(ctx, "begamin", "go.begam.in", manifest.DNSDirect, false); err == nil || !strings.Contains(err.Error(), "CNAME") {
		t.Fatalf("%v", err)
	}
	if out, err := m.Ensure(ctx, "begamin", "go.begam.in", manifest.DNSDirect, true); err != nil || out.Action != "made" {
		t.Fatalf("%+v %v", out, err)
	}
	if recs := fake.Records(); len(recs) != 1 || recs[0].Type != "A" {
		t.Fatalf("%+v", recs)
	}
}

func TestLookAndAudit(t *testing.T) {
	ctx := context.Background()
	m, fake := setup(t)
	if _, err := m.Ensure(ctx, "begamin", "api.lane.begam.in", manifest.DNSDirect, false); err != nil {
		t.Fatal(err)
	}
	fake.Seed(cloudflaretest.Record{Type: "A", Name: "*.lane.begam.in", Content: here})
	fake.Seed(cloudflaretest.Record{Type: "A", Name: "begam.in", Content: here})
	fake.Seed(cloudflaretest.Record{Type: "A", Name: "old.lane.begam.in", Content: here, Comment: "bedrock: gone on bedrock-lane"})
	fake.Seed(cloudflaretest.Record{Type: "A", Name: "hello.lane.begam.in", Content: "198.51.100.7"})
	routes := []Route{
		{App: "begamin", Host: "api.lane.begam.in", Mode: manifest.DNSDirect},
		{App: "hello", Host: "hello.lane.begam.in"},
		{App: "site", Host: "site.lane.begam.in"},
		{App: "shop", Host: "shop.nowhere.test"},
	}
	statuses, err := m.Look(ctx, routes)
	if err != nil {
		t.Fatal(err)
	}
	verdicts := map[string]string{}
	for _, s := range statuses {
		verdicts[s.Host] = s.Verdict
	}
	if verdicts["api.lane.begam.in"] != "ok" || verdicts["hello.lane.begam.in"] != "points elsewhere" || verdicts["site.lane.begam.in"] != "covered by *.lane.begam.in, which points here" || verdicts["shop.nowhere.test"] != "not in a zone this token sees" {
		t.Fatalf("%v", verdicts)
	}
	findings, err := m.Audit(ctx, routes)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, f := range findings {
		lines = append(lines, f.Host+": "+f.What)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"begam.in: points at this machine without a route: nothing answers for it",
		"old.lane.begam.in: made by bedrock for gone on bedrock-lane, which no longer routes it",
		"hello.lane.begam.in: routed by hello here but points at 198.51.100.7",
		"shop.nowhere.test: routed by shop but in no zone this token sees",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("audit lacks %q:\n%s", want, joined)
		}
	}
	for _, not := range []string{"*.lane.begam.in", "api.lane.begam.in:", "site.lane.begam.in"} {
		if strings.Contains(joined, not) {
			t.Errorf("audit shouldn't mention %q:\n%s", not, joined)
		}
	}
}

func TestDepthBelowTheZone(t *testing.T) {
	for host, want := range map[string]int{"api.begam.in": 1, "api.lane.begam.in": 2, "begam.in": 0} {
		if got := (Outcome{Host: host, Zone: "begam.in"}).Depth(); got != want {
			t.Errorf("%s: depth %d, want %d", host, got, want)
		}
	}
}

func TestARecordPointedElsewhereByHandStaysWhenTheAppLeaves(t *testing.T) {
	ctx := context.Background()
	m, fake := setup(t)
	// bedrock's own comment is still on it, but someone moved it since.
	fake.Seed(cloudflaretest.Record{Type: "A", Name: "site.begam.in", Content: "203.0.113.9", Comment: "bedrock: site on bedrock-lane"})
	removed, err := m.Remove(ctx, "site", []string{"site.begam.in"})
	if err != nil || len(removed) != 0 {
		t.Fatalf("removed %v %v", removed, err)
	}
	if recs := fake.Records(); len(recs) != 1 {
		t.Fatalf("the moved record went: %+v", recs)
	}
}

func TestPointingSaysWhatItTookOutOfTheWay(t *testing.T) {
	ctx := context.Background()
	m, fake := setup(t)
	fake.Seed(cloudflaretest.Record{Type: "CNAME", Name: "www.begam.in", Content: "begam.in"})
	out, err := m.Ensure(ctx, "site", "www.begam.in", manifest.DNSDirect, true)
	if err != nil || out.Action != "made" || strings.Join(out.Removed, ",") != "CNAME begam.in" {
		t.Fatalf("%+v %v", out, err)
	}
	if !strings.Contains(out.String(), "removed CNAME begam.in") {
		t.Fatalf("the outcome doesn't say: %s", out)
	}
}
