package app

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"strings"

	"github.com/kylebegeman/bedrock/internal/manifest"
)

// PreviewSuffix separates a parent app's name from its branch.
const PreviewSuffix = "-pr-"

// maxAppName is what a manifest accepts for an app's name.
const maxAppName = 40

// PreviewOf derives a throwaway copy of an app for one branch.
//
// A preview is an ordinary app in every way that matters: it has its own
// name, its own containers, its own database and its own secrets, and it is
// deployed, backed up against and removed by the same code as anything
// else. What makes it a preview is what this function changes.
//
// Every route is put behind a sign-in, and there is no flag to turn it off.
// A preview is unreleased work at a hostname anyone can work out from a
// branch name, and it runs with the parent's hand-set secrets so it can
// start at all. Either is reason enough; together they make an open
// preview indefensible.
//
// Its database starts empty and is built by whatever the app runs to
// migrate itself, rather than copied from production. A preview is for
// seeing a branch work, and most branches need a schema rather than
// somebody's real rows.
//
// Checks are dropped. A deploy runs them twice, once against the containers
// and once through the edge, with the same expected status both times. The
// second one now arrives at a guarded route and is answered 401, which
// would fail the deploy and roll it back. Keeping a check that cannot pass
// would make every preview look broken.
//
// Backups are turned off. The data is a copy of something already backed
// up, and a preview exists to be thrown away.
func PreviewOf(parent *manifest.Manifest, branch, domain string) (*manifest.Manifest, error) {
	label := DNSLabel(branch)
	if label == "" {
		return nil, fmt.Errorf("%q has nothing in it a hostname can use", branch)
	}
	if !manifest.ValidHost(domain) {
		return nil, fmt.Errorf("%q isn't a domain to put previews under, such as preview.example.com", domain)
	}

	// Nothing is shared with the parent: its manifest is still in use, and
	// nothing here may be rewritten underneath it.
	preview := *parent
	preview.App = previewName(parent.App, branch)
	preview.Description = fmt.Sprintf("Preview of %s at %s", parent.App, branch)
	preview.Checks = nil
	preview.Backup = &manifest.Backup{Off: true}
	if parent.Backup != nil && parent.Backup.Keep != nil {
		keep := *parent.Backup.Keep
		preview.Backup.Keep = &keep
	}
	if parent.Data != nil {
		data := *parent.Data
		if data.Postgres != nil {
			pg := *data.Postgres
			pg.Env, pg.Secrets = maps.Clone(pg.Env), append([]string(nil), pg.Secrets...)
			data.Postgres = &pg
		}
		if data.Objects != nil {
			objects := *data.Objects
			data.Objects = &objects
		}
		data.Volumes = maps.Clone(data.Volumes)
		preview.Data = &data
	}
	if parent.Secrets != nil {
		secrets := *parent.Secrets
		secrets.Generate, secrets.Derive = maps.Clone(secrets.Generate), maps.Clone(secrets.Derive)
		preview.Secrets = &secrets
	}
	preview.Workloads = make(map[string]manifest.Workload, len(parent.Workloads))
	claimed := map[string]bool{}
	for name, w := range parent.Workloads {
		copied := w
		copied.Routes = nil
		for _, r := range w.Routes {
			host := previewHost(r.Host, branch, domain)
			if !manifest.ValidHost(host) {
				return nil, fmt.Errorf("%s would become %q, which isn't a hostname; use a shorter branch name", r.Host, host)
			}
			key := host + " " + r.NormalizedPath()
			if claimed[key] {
				// Two of the parent's hostnames collapsed onto one, which
				// is what happens to an app and its www alias. One route is
				// all a preview needs.
				continue
			}
			claimed[key] = true
			route := r
			route.Host = host
			route.Auth = manifest.AuthLoom
			copied.Routes = append(copied.Routes, route)
		}
		preview.Workloads[name] = copied
	}
	if err := preview.Validate(); err != nil {
		return nil, fmt.Errorf("the preview derived from %s is not a valid app, which is a bug in bedrock: %w", parent.App, err)
	}
	return &preview, nil
}

// SetRouteDNS makes every route of an app keep its record one way, for a
// preview whose parent's routes say another: a parent whose records are
// kept by hand has none for a preview's hostnames.
func SetRouteDNS(m *manifest.Manifest, mode manifest.DNSMode) {
	for name, w := range m.Workloads {
		routes := make([]manifest.Route, len(w.Routes))
		for i, r := range w.Routes {
			r.DNS = mode
			routes[i] = r
		}
		w.Routes = routes
		m.Workloads[name] = w
	}
}

// previewName is what a branch's preview of an app is called. It is
// derived rather than chosen so that the same branch always maps to the
// same app, and deploying a branch twice updates it instead of making a
// second one.
func previewName(app, branch string) string {
	name := app + PreviewSuffix + DNSLabel(branch)
	if len(name) <= maxAppName {
		return name
	}
	// Too long to keep whole. Keep as much as will fit and end with a
	// digest of the full name, so two long branches of the same app cannot
	// land on one preview.
	sum := sha256.Sum256([]byte(name))
	tail := "-" + hex.EncodeToString(sum[:])[:6]
	return strings.TrimRight(name[:maxAppName-len(tail)], "-") + tail
}

// previewHost is where a branch's copy of one of an app's hostnames
// answers: the branch, the first label of the original hostname, and the
// domain previews live under.
//
// The original's first label is kept so that an app serving several
// hostnames keeps them apart. Collapsing loom.example.com and
// runner.loom.example.com onto one preview hostname would put two
// workloads on the same route.
func previewHost(host, branch, domain string) string {
	first, _, _ := strings.Cut(host, ".")
	return DNSLabel(branch) + "." + DNSLabel(first) + "." + domain
}

// DNSLabel turns a branch name into something that can be one label of a
// hostname: lowercase, digits and hyphens, never starting or ending with
// one. A slash is as common in a branch name as a letter, so this is not a
// validation but a conversion.
func DNSLabel(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	// A label is at most 63 octets, and one that long in a branch name is
	// a description rather than a name.
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-")
	}
	return out
}

// IsPreview reports whether an app is a preview of another one, and of
// which.
func IsPreview(app string) (parent string, ok bool) {
	i := strings.LastIndex(app, PreviewSuffix)
	if i <= 0 {
		return "", false
	}
	return app[:i], true
}
