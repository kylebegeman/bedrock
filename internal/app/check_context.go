package app

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/kylebegeman/bedrock/internal/docker"
	"github.com/kylebegeman/bedrock/internal/manifest"
)

type checkInspector interface {
	Inspect(context.Context, string) (*docker.Info, error)
}

type verification struct {
	application []manifest.Check
	edge        []manifest.Check
	guards      []manifest.Check
}

// verificationChecks keeps app content assertions separate from the edge's
// refusal of anonymous requests. An explicit edge check may carry credentials;
// an application check and a guard probe never do.
func verificationChecks(m *manifest.Manifest) verification {
	var checks verification
	for _, c := range m.Checks {
		if c.Context != manifest.CheckEdge {
			checks.application = append(checks.application, c)
		}
		if c.Context == manifest.CheckApplication {
			continue
		}
		u, err := url.Parse(c.URL)
		guarded := false
		if err == nil {
			_, _, route, ok := m.RouteFor(u.Hostname(), u.Path)
			guarded = ok && route.Auth.Guarded()
		}
		if c.Context == manifest.CheckEdge || !guarded {
			checks.edge = append(checks.edge, c)
		}
	}
	seen := map[string]bool{}
	for _, name := range m.WorkloadNames() {
		for _, route := range m.Workloads[name].Routes {
			if !route.Auth.Guarded() {
				continue
			}
			path := strings.TrimSuffix(route.NormalizedPath(), "/")
			if path == "" {
				path = "/"
			}
			target := "https://" + route.Host + path
			if !seen[target] {
				checks.guards = append(checks.guards, manifest.Check{URL: target})
				seen[target] = true
			}
		}
	}
	return checks
}

// CheckSecretNames names the credentials verification requires, without opening
// the secrets store. The watcher uses this to load the active revision's version.
func CheckSecretNames(m *manifest.Manifest) []string {
	seen := map[string]bool{}
	for _, c := range m.Checks {
		for _, name := range c.HeadersFromSecrets {
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func checkVerificationSecrets(m *manifest.Manifest, values map[string]string) error {
	for _, name := range CheckSecretNames(m) {
		if values[name] == "" {
			return fmt.Errorf("verification secret %s is not set; bedrock secret set %s %s", name, m.App, name)
		}
	}
	return nil
}

// RunAppChecks runs the same assertions used by deploys against an active app.
// Credential values come from the active revision's pinned secrets version.
func RunAppChecks(ctx context.Context, e *docker.Engine, m *manifest.Manifest, containers, values map[string]string) []error {
	checks := verificationChecks(m)
	var failures []error
	for _, c := range checks.application {
		if err := runCheckDirect(ctx, e, m, containers, c); err != nil {
			failures = append(failures, err)
		}
	}
	for _, c := range checks.guards {
		if err := runGuardCheck(ctx, c); err != nil {
			failures = append(failures, err)
		}
	}
	for _, c := range checks.edge {
		if err := runEdgeCheck(ctx, c, values); err != nil {
			failures = append(failures, err)
		}
	}
	return failures
}

func runEdgeCheck(ctx context.Context, c manifest.Check, values map[string]string) error {
	if len(c.HeadersFromSecrets) == 0 {
		return runCheck(ctx, c)
	}
	transport := publicTransport()
	defer transport.CloseIdleConnections()
	return credentialCheck(ctx, c, values, transport)
}

func credentialCheck(ctx context.Context, c manifest.Check, values map[string]string, transport http.RoundTripper) error {
	if c.Context != manifest.CheckEdge {
		return fmt.Errorf("check %s: credentials require context: edge", c.URL)
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("credential check requires an https url without embedded credentials")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return fmt.Errorf("credential check could not build its request")
	}
	for _, header := range sortedKeys(c.HeadersFromSecrets) {
		name := c.HeadersFromSecrets[header]
		value := values[name]
		if !manifest.ValidCheckHeader(header) {
			return fmt.Errorf("check %s: %s is not an allowed request header", c.URL, header)
		}
		if value == "" {
			return fmt.Errorf("check %s: verification secret %s is not set", c.URL, name)
		}
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("check %s: verification secret %s cannot be used as a request header", c.URL, name)
		}
		req.Header.Set(header, value)
	}
	within := checkTimeout(c)
	client := &http.Client{Timeout: within, Transport: transport, CheckRedirect: func(next *http.Request, via []*http.Request) error {
		// Go forwards some custom headers on redirects even when it strips
		// Authorization. Refuse every origin change before any header travels.
		if !strings.EqualFold(next.URL.Scheme, u.Scheme) || !strings.EqualFold(next.URL.Host, u.Host) || next.URL.User != nil {
			return fmt.Errorf("credential check refused a redirect to another origin")
		}
		if len(via) >= 10 {
			return fmt.Errorf("credential check stopped after ten redirects")
		}
		return nil
	}}
	err = evaluateCheck(c, client, req, within)
	if err != nil {
		message := err.Error()
		for _, name := range c.HeadersFromSecrets {
			if value := values[name]; value != "" {
				message = strings.ReplaceAll(message, value, "[redacted]")
			}
		}
		return fmt.Errorf("%s", message)
	}
	return nil
}

func runGuardCheck(ctx context.Context, c manifest.Check) error {
	transport := publicTransport()
	defer transport.CloseIdleConnections()
	return guardCheck(ctx, c, transport)
}

func guardCheck(ctx context.Context, c manifest.Check, transport http.RoundTripper) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return fmt.Errorf("guard %s: %w", c.URL, err)
	}
	client := &http.Client{Timeout: checkTimeout(c), Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("guard %s: %w", c.URL, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil
	case http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		location, err := resp.Location()
		if err == nil && (location.Scheme == "https" || location.Scheme == "http") && location.Host != "" && location.User == nil {
			location.Fragment = ""
			if location.String() != req.URL.String() {
				return nil
			}
		}
	}
	return fmt.Errorf("guard %s: anonymous request answered %s; want 401, 403 or a sign-in redirect", c.URL, resp.Status)
}
