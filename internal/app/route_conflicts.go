package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kylebegeman/bedrock/internal/manifest"
	"github.com/kylebegeman/bedrock/internal/state"
)

// CheckRouteConflicts refuses exact host/path ownership conflicts before a
// deploy builds or runs releases. Different prefixes may share one host, but
// that host's DNS mode must agree. The app may replace its own active routes.
func CheckRouteConflicts(ctx context.Context, store *state.Store, m *manifest.Manifest) error {
	if store == nil || len(m.Hosts()) == 0 {
		return nil
	}
	active, err := store.ActiveRevisions(ctx)
	if err != nil {
		return err
	}
	var clashes []string
	for _, rev := range active {
		if rev.App == m.App {
			continue
		}
		var other manifest.Manifest
		if err := json.Unmarshal(rev.Manifest, &other); err != nil {
			return fmt.Errorf("revision %s/%s manifest: %w", rev.App, rev.ID, err)
		}
		for _, name := range m.WorkloadNames() {
			for _, mine := range m.Workloads[name].Routes {
				for _, otherName := range other.WorkloadNames() {
					for _, theirs := range other.Workloads[otherName].Routes {
						if mine.Host != theirs.Host {
							continue
						}
						if mine.NormalizedPath() == theirs.NormalizedPath() {
							clashes = append(clashes, fmt.Sprintf("%s%s for %s is already routed to %s's %s", mine.Host, mine.NormalizedPath(), name, other.App, otherName))
						} else if mine.DNS != theirs.DNS {
							clashes = append(clashes, fmt.Sprintf("%s uses dns %q here and %q in %s; routes sharing a host must use one DNS mode", mine.Host, mine.DNS, theirs.DNS, other.App))
						}
					}
				}
			}
		}
	}
	if len(clashes) > 0 {
		return fmt.Errorf("%s can't route what conflicts with another app on this machine: %s", m.App, strings.Join(clashes, "; "))
	}
	return nil
}
