package edge

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestConfigGroupsByHostAndOrdersPrefixes(t *testing.T) {
	cfg, err := Config([]Route{
		{Host: "kylebegeman.com", Path: "/", Dial: "bedrock-kylebegeman-site-abc:8080"},
		{Host: "kylebegeman.com", Path: "/thebatteredbaker/", Dial: "bedrock-kylebegeman-bakery-abc:8383"},
		{Host: "www.kylebegeman.com", Path: "/", Dial: "bedrock-kylebegeman-site-abc:8080"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Admin struct {
			Listen string `json:"listen"`
		} `json:"admin"`
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Listen []string `json:"listen"`
					Routes []struct {
						Match []struct {
							Host []string `json:"host"`
						} `json:"match"`
						Handle []struct {
							Handler string `json:"handler"`
							Routes  []struct {
								Match []struct {
									Path []string `json:"path"`
								} `json:"match"`
								Handle []struct {
									Handler   string `json:"handler"`
									Upstreams []struct {
										Dial string `json:"dial"`
									} `json:"upstreams"`
								} `json:"handle"`
							} `json:"routes"`
						} `json:"handle"`
						Terminal bool `json:"terminal"`
					} `json:"routes"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(cfg, &parsed); err != nil {
		t.Fatal(err)
	}
	// Never a network address: an app on an edge network must not reach it.
	if parsed.Admin.Listen != "unix//run/bedrock/caddy.sock" {
		t.Fatalf("admin: %+v", parsed.Admin)
	}
	server := parsed.Apps.HTTP.Servers["bedrock"]
	if strings.Join(server.Listen, ",") != ":80,:443" || len(server.Routes) != 2 {
		t.Fatalf("server: %+v", server)
	}
	first := server.Routes[0]
	if first.Match[0].Host[0] != "kylebegeman.com" || !first.Terminal || first.Handle[0].Handler != "subroute" {
		t.Fatalf("first route: %+v", first)
	}
	sub := first.Handle[0].Routes
	if len(sub) != 2 || strings.Join(sub[0].Match[0].Path, ",") != "/thebatteredbaker,/thebatteredbaker/*" || sub[0].Handle[0].Upstreams[0].Dial != "bedrock-kylebegeman-bakery-abc:8383" {
		t.Fatalf("prefix route first: %+v", sub[0])
	}
	if len(sub[1].Match) != 0 || sub[1].Handle[0].Upstreams[0].Dial != "bedrock-kylebegeman-site-abc:8080" {
		t.Fatalf("catch-all last: %+v", sub[1])
	}
	if server.Routes[1].Match[0].Host[0] != "www.kylebegeman.com" {
		t.Fatalf("second host: %+v", server.Routes[1])
	}
}

func TestInitialServesNothingButAnswersAdmin(t *testing.T) {
	var parsed map[string]any
	if err := json.Unmarshal(Initial(), &parsed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(Initial()), `"routes": []`) {
		t.Fatalf("initial config:\n%s", Initial())
	}
}

// Every handler that reaches an app keeps its upgraded connections for a
// while after a reload, guarded or not; the guard's own probe is never
// upgraded and has no reason to.
func TestUpgradedConnectionsOutliveAReload(t *testing.T) {
	cfg, err := Config([]Route{
		{Host: "site.example.com", Path: "/", Dial: "bedrock-site-abc:8080"},
		{Host: "site.example.com", Path: "/ws/", Dial: "bedrock-site-abc:8081"},
		{Host: "admin.example.com", Path: "/", Dial: "bedrock-admin-abc:8000",
			Guard: &Guard{Dial: "loom.example.com:443", Path: "/verify", TLS: true, HeaderName: "X-Loom-Verified", HeaderValue: "1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var tree any
	if err := json.Unmarshal(cfg, &tree); err != nil {
		t.Fatal(err)
	}
	delays := map[string]any{}
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if v["handler"] == "reverse_proxy" {
				dial := v["upstreams"].([]any)[0].(map[string]any)["dial"].(string)
				delays[dial] = v["stream_close_delay"]
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(tree)
	for _, dial := range []string{"bedrock-site-abc:8080", "bedrock-site-abc:8081", "bedrock-admin-abc:8000"} {
		if delays[dial] != StreamCloseDelay {
			t.Errorf("%s: stream_close_delay %v, want %s", dial, delays[dial], StreamCloseDelay)
		}
	}
	if delay, found := delays["loom.example.com:443"]; !found || delay != nil {
		t.Errorf("the guard's probe: stream_close_delay %v (found %v), want none", delay, found)
	}
	if _, err := time.ParseDuration(StreamCloseDelay); err != nil {
		t.Fatalf("Caddy reads the delay as a Go duration: %v", err)
	}
}
