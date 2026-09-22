package edge

import (
	"encoding/json"
	"strings"
	"testing"
)

func configOf(t *testing.T, routes []Route) map[string]any {
	t.Helper()
	raw, err := Config(routes)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The handler that reaches the app has to sit under the 2xx match, not
// beside it. Beside it would serve the app to everyone and ask the guard
// pointlessly, which is the failure this test exists to catch.
func TestAGuardedRouteReachesTheAppOnlyUnderATwoHundred(t *testing.T) {
	cfg := configOf(t, []Route{{
		Host: "admin.example.com", Path: "/", Dial: "bedrock-admin-abc:8000",
		Guard: &Guard{Dial: "loom.example.com:443", Path: "/api/cloud/auth/verify", TLS: true, ServerName: "loom.example.com"},
	}})
	raw, _ := json.Marshal(cfg)
	text := string(raw)

	if !strings.Contains(text, "handle_response") {
		t.Fatalf("no forward auth in the configuration: %s", text)
	}
	// The app's upstream must not be reachable except through the guard's
	// 2xx branch, so it appears inside handle_response and nowhere else.
	before, after, found := strings.Cut(text, "handle_response")
	if !found {
		t.Fatal("no handle_response")
	}
	if strings.Contains(before, "bedrock-admin-abc:8000") {
		t.Fatalf("the app is reachable before the guard answers: %s", before)
	}
	if !strings.Contains(after, "bedrock-admin-abc:8000") {
		t.Fatalf("the app is not reachable after a 2xx: %s", after)
	}
	if !strings.Contains(after, `"status_code":[2]`) {
		t.Fatalf("the allow branch is not gated on 2xx: %s", after)
	}
	// The probe must not be the original request replayed.
	if !strings.Contains(text, `"method":"GET"`) || !strings.Contains(text, "/api/cloud/auth/verify") {
		t.Fatalf("the probe is not a GET to the verify endpoint: %s", text)
	}
	if !strings.Contains(text, "X-Forwarded-Uri") {
		t.Fatal("the guard is not told what was asked for")
	}
	if !strings.Contains(text, `"server_name":"loom.example.com"`) {
		t.Fatalf("an https guard must verify the name it dials: %s", text)
	}
}

// An unguarded route must be exactly what it was before guards existed.
func TestAnUnguardedRouteIsUntouched(t *testing.T) {
	raw, err := Config([]Route{{Host: "site.example.com", Path: "/", Dial: "bedrock-site-abc:8080"}})
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, "handle_response") || strings.Contains(text, "X-Forwarded-Uri") {
		t.Fatalf("an open route grew a guard: %s", text)
	}
	if !strings.Contains(text, "bedrock-site-abc:8080") {
		t.Fatalf("the upstream went missing: %s", text)
	}
}

// Guarding one route on a host must not guard its neighbours, and must not
// stop being guarded because a neighbour is not.
func TestGuardsArePerRouteNotPerHost(t *testing.T) {
	guard := &Guard{Dial: "loom.example.com:443", Path: "/verify", TLS: true}
	cfg := configOf(t, []Route{
		{Host: "app.example.com", Path: "/", Dial: "bedrock-app-pub:8000"},
		{Host: "app.example.com", Path: "/admin/", Dial: "bedrock-app-adm:8000", Guard: guard},
	})
	raw, _ := json.Marshal(cfg)
	if n := strings.Count(string(raw), "handle_response"); n != 1 {
		t.Fatalf("want exactly one guarded route, got %d: %s", n, raw)
	}
	// Walk the host's subroutes: the open one must dial the app directly,
	// and the guarded one must not.
	var openDirect, adminDirect bool
	for _, sub := range subroutes(t, cfg, "app.example.com") {
		handles, _ := sub["handle"].([]any)
		for _, h := range handles {
			m, _ := h.(map[string]any)
			if m["handler"] != "reverse_proxy" {
				continue
			}
			for _, u := range m["upstreams"].([]any) {
				switch u.(map[string]any)["dial"] {
				case "bedrock-app-pub:8000":
					openDirect = true
				case "bedrock-app-adm:8000":
					adminDirect = true
				}
			}
		}
	}
	if !openDirect {
		t.Fatal("the open route must still reach its app directly")
	}
	if adminDirect {
		t.Fatal("the guarded route must not be reachable without the guard")
	}
}

// subroutes digs out one host's route list.
func subroutes(t *testing.T, cfg map[string]any, host string) []map[string]any {
	t.Helper()
	servers := cfg["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)
	routes := servers["bedrock"].(map[string]any)["routes"].([]any)
	for _, r := range routes {
		m := r.(map[string]any)
		match := m["match"].([]any)[0].(map[string]any)
		if match["host"].([]any)[0] != host {
			continue
		}
		handle := m["handle"].([]any)[0].(map[string]any)
		var out []map[string]any
		for _, sub := range handle["routes"].([]any) {
			out = append(out, sub.(map[string]any))
		}
		return out
	}
	t.Fatalf("no routes for %s", host)
	return nil
}

// A plain http guard needs no TLS transport, and must not claim one.
func TestAPlainGuardHasNoTLSTransport(t *testing.T) {
	raw, err := Config([]Route{{
		Host: "app.example.com", Path: "/", Dial: "bedrock-app-abc:8000",
		Guard: &Guard{Dial: "127.0.0.1:4773", Path: "/verify"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "\"tls\"") {
		t.Fatalf("a plain guard must not ask for TLS: %s", raw)
	}
}
