package app

import (
	"fmt"
	"strings"

	"github.com/kylebegeman/bedrock/internal/manifest"
)

// CloudflareOnly reports whether this machine lets only Cloudflare's proxy
// reach 80 and 443 (host setup --web-from cloudflare). Nil means it does
// not; the daemon reads the machine's profile.
type CloudflareOnly func() (bool, error)

// bypassing lists the hosts that would reach the machine without going
// through Cloudflare's proxy, when the machine answers only Cloudflare;
// none otherwise.
func (only CloudflareOnly) bypassing(hosts map[string]manifest.DNSMode) ([]string, error) {
	if only == nil || len(hosts) == 0 {
		return nil, nil
	}
	yes, err := only()
	if err != nil {
		return nil, fmt.Errorf("read who this machine takes the web from: %w", err)
	}
	if !yes {
		return nil, nil
	}
	var direct []string
	for _, host := range sortedKeys(hosts) {
		if mode := hosts[host]; mode != manifest.DNSProxied {
			direct = append(direct, fmt.Sprintf("%s (dns: %s)", host, modeName(mode)))
		}
	}
	return direct, nil
}

// onlyCloudflare opens the refusals below.
const onlyCloudflare = "this machine takes the web only from Cloudflare (bedrock host setup --web-from cloudflare)"

// checkRoutes refuses a manifest with a route Cloudflare's proxy would not
// carry, on a machine that answers only Cloudflare.
func (only CloudflareOnly) checkRoutes(m *manifest.Manifest) error {
	direct, err := only.bypassing(routeModes(m))
	if err != nil || len(direct) == 0 {
		return err
	}
	return fmt.Errorf("%s, so nothing could reach %s's %s. Give every route dns: proxied, or open the web to anyone with bedrock host setup --web-from anyone",
		onlyCloudflare, m.App, strings.Join(direct, ", "))
}

// routeModes is every host a manifest routes, with its record mode.
func routeModes(m *manifest.Manifest) map[string]manifest.DNSMode {
	managed := m.ManagedHosts()
	out := map[string]manifest.DNSMode{}
	for _, host := range m.Hosts() {
		out[host] = managed[host]
	}
	return out
}

func modeName(mode manifest.DNSMode) string {
	if mode == manifest.DNSManual {
		return "manual"
	}
	return string(mode)
}
