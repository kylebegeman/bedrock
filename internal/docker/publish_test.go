package docker

import "testing"

// A published port reaches Docker as a string, and an IPv6 address has
// colons of its own, so it has to arrive in brackets and still be read.
func TestPublishReadsEveryAddressForm(t *testing.T) {
	for _, c := range []struct {
		spec, port, hostIP, hostPort string
	}{
		{"3478:3478/udp", "3478/udp", "", "3478"},
		{"8443:443", "443/tcp", "", "8443"},
		{"127.0.0.1:2019:2019/tcp", "2019/tcp", "127.0.0.1", "2019"},
		{"[::]:3478:3478/udp", "3478/udp", "::", "3478"},
		{"[2001:db8::1]:8443:443/tcp", "443/tcp", "2001:db8::1", "8443"},
	} {
		port, binding, err := parsePublish(c.spec)
		if err != nil {
			t.Errorf("%s: %v", c.spec, err)
			continue
		}
		hostIP := ""
		if binding.HostIP.IsValid() {
			hostIP = binding.HostIP.String()
		}
		if port.String() != c.port || hostIP != c.hostIP || binding.HostPort != c.hostPort {
			t.Errorf("%s: %s on %q:%s, want %s on %q:%s", c.spec, port, hostIP, binding.HostPort, c.port, c.hostIP, c.hostPort)
		}
	}
	for _, bad := range []string{"3478", "::1:8443:443/tcp", "[::1]8443:443", "[::1]:1:2:3/tcp", "a:b:c:d"} {
		if _, _, err := parsePublish(bad); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}
