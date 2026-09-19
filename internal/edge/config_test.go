package edge

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConfigGroupsByHostAndOrdersPrefixes(t *testing.T) {
	cfg, err := Config([]Route{
		{Host: "kylebegeman.com", Path: "/", Dial: "quark-kylebegeman-site-abc:8080"},
		{Host: "kylebegeman.com", Path: "/thebatteredbaker/", Dial: "quark-kylebegeman-bakery-abc:8383"},
		{Host: "www.kylebegeman.com", Path: "/", Dial: "quark-kylebegeman-site-abc:8080"},
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
	if parsed.Admin.Listen != ":2019" {
		t.Fatalf("admin: %+v", parsed.Admin)
	}
	server := parsed.Apps.HTTP.Servers["quark"]
	if strings.Join(server.Listen, ",") != ":80,:443" || len(server.Routes) != 2 {
		t.Fatalf("server: %+v", server)
	}
	first := server.Routes[0]
	if first.Match[0].Host[0] != "kylebegeman.com" || !first.Terminal || first.Handle[0].Handler != "subroute" {
		t.Fatalf("first route: %+v", first)
	}
	sub := first.Handle[0].Routes
	if len(sub) != 2 || strings.Join(sub[0].Match[0].Path, ",") != "/thebatteredbaker,/thebatteredbaker/*" || sub[0].Handle[0].Upstreams[0].Dial != "quark-kylebegeman-bakery-abc:8383" {
		t.Fatalf("prefix route first: %+v", sub[0])
	}
	if len(sub[1].Match) != 0 || sub[1].Handle[0].Upstreams[0].Dial != "quark-kylebegeman-site-abc:8080" {
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
