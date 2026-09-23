// Package edge is the web front door: one Caddy on ports 80 and 443 whose
// whole configuration bedrock owns and rebuilds from the apps it runs.
package edge

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Route sends one host and path prefix to a container.
type Route struct {
	Host string
	// Path is a normalized prefix ending in "/"; "/" is the catch-all.
	Path string
	// Dial is the upstream as Caddy reaches it on the edge network,
	// such as bedrock-kylebegeman-site-3f2a9c:8080.
	Dial string
	// Guard, when set, is asked about every request before the upstream
	// sees it. A route with no Guard is open.
	Guard *Guard
}

// Guard is an endpoint the edge asks whether a request may proceed.
//
// The answer is the status code and nothing else: any 2xx lets the request
// through to the app, and anything else is returned to the browser as it
// stands. That is what makes a redirect to a sign-in page work without the
// edge knowing anything about signing in.
//
// Nothing of the original request's body is sent, and the probe is a GET,
// so asking is cheap and cannot have side effects of its own.
type Guard struct {
	// Dial is host:port as Caddy reaches the endpoint.
	Dial string
	// Path is the endpoint's path, such as /api/cloud/auth/verify.
	Path string
	// TLS says the endpoint is reached over HTTPS.
	TLS bool
	// ServerName is the name to present and verify when TLS is on, for an
	// endpoint dialled by address rather than by name.
	ServerName string
	// HeaderName and HeaderValue are a header the answer must carry before
	// a 2xx counts as a yes.
	//
	// A status code on its own is not enough. An application that serves a
	// single-page app answers 200 with its index for any path it does not
	// recognise, so a guard pointed at a host that has no verify endpoint
	// at all, or one that has been rolled back to before it existed, would
	// read that 200 as permission and let everybody through. The header is
	// something only the endpoint itself sets, so a fallback cannot say
	// yes by accident.
	HeaderName  string
	HeaderValue string
}

// StreamCloseDelay is how long a connection upgraded through the edge, such
// as a WebSocket or Tailscale's control protocol, outlives the configuration
// it was opened under.
//
// Every deploy and every daemon start loads a whole new configuration, and
// Caddy's default is to close every upgraded connection the moment the old
// one is unloaded, so a deploy of any one app would cut every client of
// every app at the same instant and have them all reconnect together. With
// a delay, a stream that ends on its own within five minutes never notices
// the reload, and the ones still open are closed then rather than in the
// middle of the deploy that caused it. A deploy reloads more than once, so
// the delay is minutes rather than seconds, and it is bounded because the
// streams are closed in the end all the same: clients such as Tailscale's
// reconnect on their own. The guard's probe is never upgraded and has none.
const StreamCloseDelay = "5m"

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
			handle := []map[string]any{{"handler": "reverse_proxy", "upstreams": []map[string]any{{"dial": r.Dial}}, "stream_close_delay": StreamCloseDelay}}
			if r.Guard != nil {
				// A guard that decides on the status alone can be talked
				// into a yes by any server that answers 200 for a path it
				// does not know, which is what a single-page app does. There
				// is no configuration in which that is wanted, so it is not
				// one that can be built.
				if r.Guard.HeaderName == "" {
					return nil, fmt.Errorf("the guard on %s%s names no header to require; a status alone can be answered by anything", r.Host, r.Path)
				}
				handle = []map[string]any{guardHandler(r.Guard, handle)}
			}
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
		// the edge share. bedrock keeps the configuration itself (BootFile),
		// so Caddy's own copy is off.
		"admin": map[string]any{"listen": adminListen, "config": map[string]any{"persist": false}},
		"logging": map[string]any{"logs": map[string]any{
			"default": map[string]any{"writer": map[string]any{"output": "stdout"}, "encoder": map[string]any{"format": "json"}},
		}},
		"apps": map[string]any{
			"http": map[string]any{
				// Per-host counters and latency histograms on the admin
				// API's /metrics: the traffic signals bedrock keeps per app.
				"metrics": map[string]any{"per_host": true},
				"servers": map[string]any{
					"bedrock": map[string]any{
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
// deployed: an admin API bedrock can reach, and nothing to serve.
//
// A route's Dial may be a Unix socket (unix//path), for bedrock's own
// endpoints.
func Initial() []byte {
	b, _ := Config(nil)
	return b
}

// guardHandler asks the guard about the request and, only on a 2xx, runs
// the handlers that reach the app.
//
// This is Caddy's forward_auth written out: a reverse_proxy to the guard
// whose response is handled here rather than returned, with the app's own
// handlers nested under a 2xx match. Anything the guard answers that is not
// a 2xx is copied back to the browser untouched, which is how a 302 to a
// sign-in page reaches the person rather than the app.
func guardHandler(g *Guard, allowed []map[string]any) map[string]any {
	upstream := map[string]any{"dial": g.Dial}
	allow := map[string]any{"status_code": []int{2}}
	if g.HeaderName != "" {
		allow["headers"] = map[string]any{g.HeaderName: []string{g.HeaderValue}}
	}
	proxy := map[string]any{
		"handler":   "reverse_proxy",
		"upstreams": []map[string]any{upstream},
		// The probe carries no body and asks about, rather than performs,
		// the request; the original method and target travel as headers so
		// the guard can decide on them.
		"rewrite": map[string]any{"method": "GET", "uri": g.Path},
		"headers": map[string]any{"request": map[string]any{"set": map[string]any{
			"X-Forwarded-Method": []string{"{http.request.method}"},
			"X-Forwarded-Uri":    []string{"{http.request.uri}"},
			"X-Forwarded-Host":   []string{"{http.request.host}"},
		}}},
		"handle_response": []map[string]any{{
			"match":  allow,
			"routes": []map[string]any{{"handle": allowed}},
		}},
	}
	if g.TLS {
		tls := map[string]any{}
		if g.ServerName != "" {
			tls["server_name"] = g.ServerName
		}
		proxy["transport"] = map[string]any{"protocol": "http", "tls": tls}
	}
	return proxy
}
