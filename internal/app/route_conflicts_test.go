package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kylebegeman/bedrock/internal/manifest"
	"github.com/kylebegeman/bedrock/internal/state"
)

func routeFixture(t *testing.T, app, path string, mode manifest.DNSMode) *manifest.Manifest {
	t.Helper()
	m := &manifest.Manifest{App: app, Workloads: map[string]manifest.Workload{"web": {
		Kind: manifest.Web, Image: "example/app:1", Port: 8080,
		Routes: []manifest.Route{{Host: "shared.example.com", Path: path, DNS: mode}},
	}}}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDeployPlanRefusesAnotherAppsExactRouteBeforeApplyingAnything(t *testing.T) {
	ctx := context.Background()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	existing := routeFixture(t, "site", "/api", "")
	saveActive(t, store, existing)
	candidate := routeFixture(t, "other", "/api/", "")
	d := Deploy{Store: store, StateDir: t.TempDir()}
	_, err = d.rollout(ctx, candidate, "check", &buildFrom{source: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "shared.example.com/api/ for web is already routed to site's web") {
		t.Fatalf("a colliding route reached a deploy plan: %v", err)
	}
	ops, err := store.List(ctx, 20)
	if err != nil || len(ops) != 0 {
		t.Fatalf("planning changed the operation journal: %v %v", ops, err)
	}
}

func TestRoutePreflightAllowsOwnRoutesAndDistinctPrefixes(t *testing.T) {
	ctx := context.Background()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	saveActive(t, store, routeFixture(t, "site", "", ""))
	if err := CheckRouteConflicts(ctx, store, routeFixture(t, "site", "/", "")); err != nil {
		t.Fatalf("the app cannot redeploy its own route: %v", err)
	}
	if err := CheckRouteConflicts(ctx, store, routeFixture(t, "api", "/api", "")); err != nil {
		t.Fatalf("distinct prefixes cannot share a host: %v", err)
	}
	if err := CheckRouteConflicts(ctx, store, routeFixture(t, "api", "/api", manifest.DNSProxied)); err == nil || !strings.Contains(err.Error(), "routes sharing a host must use one DNS mode") {
		t.Fatalf("conflicting DNS ownership wasn't refused: %v", err)
	}
}

func TestRoutePlanFixtureWithoutAStoreHasNoExistingRoutes(t *testing.T) {
	if err := CheckRouteConflicts(context.Background(), nil, routeFixture(t, "site", "/", "")); err != nil {
		t.Fatal(err)
	}
}
