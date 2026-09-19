// Package edge is the web front door: one Caddy on ports 80 and 443 whose
// whole configuration quark owns and rebuilds from the apps it runs.
package edge

import (
	"encoding/json"
	"sort"
	"strings"
)

// Route sends one host and path prefix to a container.
type Route struct {
	Host string
	// Path is a normalized prefix ending in "/"; "/" is the catch-all.
	Path string
	// Dial is the upstream as Caddy reaches it on the edge network,
	// such as quark-kylebegeman-site-3f2a9c:8080.
	Dial string
}

// Config is Caddy's JSON, built from routes. Every host gets automatic
// HTTPS because it appears in a host matcher.
func Config(routes []Route) ([]byte, error) {
	byHost := map[string][]Route{}
	for _, r := range routes {
		byHost[r.Host] = append(byHost[r.Host], r)
	}
	hosts := make([]string, 0, len(byHost))
	for h := range byHost {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)

	var serverRoutes []map[string]any
	for _, host := range hosts {
		rs := byHost[host]
		// Longest prefix first; the catch-all last, without a path matcher.
		sort.Slice(rs, func(i, j int) bool { return len(rs[i].Path) > len(rs[j].Path) })
		var sub []map[string]any
		for _, r := range rs {
			handle := []map[string]any{{"handler": "reverse_proxy", "upstreams": []map[string]any{{"dial": r.Dial}}}}
			if r.Path == "/" {
				sub = append(sub, map[string]any{"handle": handle})
				continue
			}
			prefix := strings.TrimSuffix(r.Path, "/")
			sub = append(sub, map[string]any{
				"match":  []map[string]any{{"path": []string{prefix, prefix + "/*"}}},
				"handle": handle,
			})
		}
		serverRoutes = append(serverRoutes, map[string]any{
			"match":    []map[string]any{{"host": []string{host}}},
			"handle":   []map[string]any{{"handler": "subroute", "routes": sub}},
			"terminal": true,
		})
	}
	if serverRoutes == nil {
		serverRoutes = []map[string]any{}
	}
	cfg := map[string]any{
		// The admin API is a socket in a directory only this machine and
		// the edge share. quark keeps the configuration itself (BootFile),
		// so Caddy's own copy is off.
		"admin": map[string]any{"listen": adminListen, "config": map[string]any{"persist": false}},
		"logging": map[string]any{"logs": map[string]any{
			"default": map[string]any{"writer": map[string]any{"output": "stdout"}, "encoder": map[string]any{"format": "json"}},
		}},
		"apps": map[string]any{
			"http": map[string]any{
				// Per-host counters and latency histograms on the admin
				// API's /metrics: the traffic signals quark keeps per app.
				"metrics": map[string]any{"per_host": true},
				"servers": map[string]any{
					"quark": map[string]any{
						"listen": []string{":80", ":443"},
						"routes": serverRoutes,
						"logs":   map[string]any{},
					},
				},
			},
		},
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// Initial is the configuration the edge starts with before any app is
// deployed: an admin API quark can reach, and nothing to serve.
//
// A route's Dial may be a Unix socket (unix//path), for quark's own
// endpoints.
func Initial() []byte {
	b, _ := Config(nil)
	return b
}
