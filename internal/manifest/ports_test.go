package manifest

import (
	"strings"
	"testing"
)

// headscale is shaped like the control server it is named for: HTTP
// through the edge, and STUN on the machine's own address.
const headscale = `app: headscale
workloads:
  server:
    kind: web
    image: headscale/headscale:0.26
    port: 8080
    singleton: true
    routes: [{host: hs.example.com}]
    ports:
      - port: 3478
        protocol: udp
  metrics:
    kind: worker
    image: local/metrics:1
    singleton: true
    ports:
      - port: 9090
        host_port: 19090
        address: 127.0.0.1
      - port: 9091
        address: "::1"
`

func TestPublishedPortsParseWithTheirDefaults(t *testing.T) {
	m, err := Parse([]byte(headscale))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range m.PublishedPorts() {
		got = append(got, p.Workload+" "+p.String())
	}
	want := "metrics 127.0.0.1:19090/tcp,metrics [::1]:9091/tcp,server 3478/udp"
	if strings.Join(got, ",") != want {
		t.Fatalf("published %v, want %s", got, want)
	}
}

func TestPublishedPortsAreValidated(t *testing.T) {
	cases := []struct{ yaml, want string }{
		// Two containers can't hold one port, and a deploy starts the new
		// one beside the old unless it is a singleton.
		{strings.Replace(headscale, "    port: 8080\n    singleton: true\n", "    port: 8080\n", 1), "must be singleton: true"},
		{strings.Replace(headscale, "protocol: udp", "protocol: sctp", 1), `"sctp" isn't tcp or udp`},
		{strings.Replace(headscale, "- port: 3478", "- port: 70000", 1), "must be a port number"},
		{strings.Replace(headscale, "- port: 3478", "- port: 0", 1), "must be a port number"},
		{strings.Replace(headscale, "host_port: 19090", "host_port: -1", 1), "host_port: must be a port number"},
		{strings.Replace(headscale, "address: 127.0.0.1", "address: localhost", 1), "isn't an IP address"},
		{strings.Replace(headscale, "host_port: 19090", "host_port: 443", 1), "443 on the machine belongs to the edge"},
		{strings.Replace(headscale, "- port: 3478\n        protocol: udp", "- port: 80", 1), "80 on the machine belongs to the edge"},
		{strings.Replace(headscale, "- port: 3478\n        protocol: udp", "- port: 22", 1), "belongs to ssh"},
		{strings.Replace(headscale, "host_port: 19090", "host_port: 5000", 1), "bedrock's image registry"},
		// The same port and protocol twice in one app, even on two addresses.
		{strings.Replace(headscale, "host_port: 19090", "host_port: 9091", 1), "9091/tcp is already published by metrics"},
		{strings.Replace(headscale, "- port: 9091", "- port: 3478\n        protocol: udp", 1), "3478/udp is already published by metrics"},
		{strings.Replace(headscale, "  metrics:\n    kind: worker", "  metrics:\n    kind: cron\n    schedule: \"0 3 * * *\"", 1), "only web and worker workloads publish ports"},
	}
	for _, c := range cases {
		_, err := Parse([]byte(c.yaml))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("want %q, got %v", c.want, err)
		}
	}
	// One number over two protocols is two ports.
	if _, err := Parse([]byte(strings.Replace(headscale, "- port: 9091", "- port: 3478", 1))); err != nil {
		t.Fatalf("3478/tcp beside 3478/udp: %v", err)
	}
}

func TestPortsOverlapOnlyWhereTheyWouldShareASocket(t *testing.T) {
	stun := PublishedPort{Port: 3478, Protocol: "udp"}
	for _, c := range []struct {
		other PublishedPort
		want  bool
	}{
		{PublishedPort{Port: 3478, Protocol: "udp"}, true},
		{PublishedPort{Port: 3478}, false},
		{PublishedPort{Port: 1, HostPort: 3478, Protocol: "udp"}, true},
		{PublishedPort{Port: 3478, Protocol: "udp", Address: "203.0.113.4"}, true},
	} {
		if got := stun.Overlaps(c.other); got != c.want {
			t.Errorf("%s and %s: overlap %v, want %v", stun, c.other, got, c.want)
		}
	}
	a := PublishedPort{Port: 3478, Protocol: "udp", Address: "203.0.113.4"}
	if a.Overlaps(PublishedPort{Port: 3478, Protocol: "udp", Address: "203.0.113.5"}) {
		t.Error("two addresses of the machine are two sockets")
	}
	if !a.Overlaps(PublishedPort{Port: 3478, Protocol: "udp", Address: "0.0.0.0"}) {
		t.Error("every address includes this one")
	}
}
