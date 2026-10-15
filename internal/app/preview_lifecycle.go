package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/kylebegeman/helios/internal/kernel"
	"github.com/kylebegeman/helios/internal/manifest"
	"github.com/kylebegeman/helios/internal/state"
)

const (
	PreviewPinKind   = "app.preview.pin"
	PreviewPruneKind = "app.preview.prune"
)

// PreviewTarget pins cleanup to an actual preview revision, rather than a name
// that another app or a newly deployed branch could have acquired meanwhile.
type PreviewTarget struct {
	App      string `json:"app"`
	Revision string `json:"revision"`
	Of       string `json:"of"`
	Branch   string `json:"branch"`
}

// ResolvePreview accepts a preview's name, or a parent app and an exact branch.
func ResolvePreview(ctx context.Context, store *state.Store, name, branch string) (*state.Revision, error) {
	revs, err := store.ActiveRevisions(ctx)
	if err != nil {
		return nil, err
	}
	var found *state.Revision
	for _, rev := range revs {
		var m manifest.Manifest
		if err := json.Unmarshal(rev.Manifest, &m); err != nil {
			return nil, fmt.Errorf("%s's manifest cannot be read: %w", rev.App, err)
		}
		if m.Preview == nil {
			continue
		}
		matches := branch == "" && rev.App == name || branch != "" && m.Preview.Of == name && m.Preview.Branch == branch
		if matches {
			if found != nil {
				return nil, fmt.Errorf("more than one preview of %s at %s is running; name the preview explicitly", name, branch)
			}
			copy := rev
			found = &copy
		}
	}
	if found == nil {
		if branch != "" {
			return nil, fmt.Errorf("no running preview of %s at branch %s", name, branch)
		}
		return nil, fmt.Errorf("%s is not a running preview; give its name or a parent app with --branch", name)
	}
	return found, nil
}

// ParsePreviewAge accepts Go durations and days, such as 48h and 7d.
func ParsePreviewAge(value string) (time.Duration, error) {
	text := value
	multiplier := time.Duration(1)
	if strings.HasSuffix(text, "d") {
		text = strings.TrimSuffix(text, "d") + "h"
		multiplier = 24
	}
	d, err := time.ParseDuration(text)
	if err != nil || d <= 0 || d > time.Duration(math.MaxInt64)/multiplier {
		return 0, fmt.Errorf("--older-than %q must be a positive duration such as 48h or 7d", value)
	}
	return d * multiplier, nil
}

// PrunablePreviews selects only unpinned active previews deployed before cutoff.
func PrunablePreviews(ctx context.Context, store *state.Store, parent string, cutoff time.Time) ([]PreviewTarget, error) {
	revs, err := store.ActiveRevisions(ctx)
	if err != nil {
		return nil, err
	}
	selected := []PreviewTarget{}
	for _, rev := range revs {
		var m manifest.Manifest
		if err := json.Unmarshal(rev.Manifest, &m); err != nil {
			return nil, fmt.Errorf("%s's manifest cannot be read: %w", rev.App, err)
		}
		if m.Preview == nil || m.Preview.Pinned || !rev.CreatedAt.Before(cutoff) || parent != "" && m.Preview.Of != parent {
			continue
		}
		selected = append(selected, PreviewTarget{App: rev.App, Revision: rev.ID, Of: m.Preview.Of, Branch: m.Preview.Branch})
	}
	return selected, nil
}

func PreviewTargetOf(rev *state.Revision) (PreviewTarget, error) {
	var m manifest.Manifest
	if err := json.Unmarshal(rev.Manifest, &m); err != nil {
		return PreviewTarget{}, err
	}
	if m.Preview == nil {
		return PreviewTarget{}, fmt.Errorf("%s is not a preview", rev.App)
	}
	return PreviewTarget{App: rev.App, Revision: rev.ID, Of: m.Preview.Of, Branch: m.Preview.Branch}, nil
}

type PreviewPin struct{ Store *state.Store }
type PreviewPinInput struct {
	PreviewTarget
	Pinned bool `json:"pinned"`
}

func (PreviewPin) Kind() string { return PreviewPinKind }

func (p PreviewPin) Plan(ctx context.Context, raw json.RawMessage) (*kernel.Plan, error) {
	var in PreviewPinInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	if rev, _, err := previewRevision(ctx, p.Store, in.PreviewTarget, false); err != nil {
		return nil, err
	} else if rev.Status != state.RevisionActive {
		return nil, fmt.Errorf("%s is no longer a running preview", in.App)
	}
	verb := "pin"
	if !in.Pinned {
		verb = "unpin"
	}
	return &kernel.Plan{Target: in.App, Recovery: kernel.Resume, Steps: []kernel.Step{{
		Name: verb, Change: fmt.Sprintf("%s preview %s at revision %s for cleanup", verb, in.App, in.Revision),
		Apply: func(ctx context.Context, out io.Writer) error {
			rev, _, err := previewRevision(ctx, p.Store, in.PreviewTarget, false)
			if err != nil {
				return err
			}
			if rev.Status != state.RevisionActive {
				return fmt.Errorf("%s is no longer a running preview", in.App)
			}
			revisions, err := p.Store.Revisions(ctx, in.App)
			if err != nil {
				return err
			}
			var updates []state.Revision
			for _, historical := range revisions {
				var stored manifest.Manifest
				if err := json.Unmarshal(historical.Manifest, &stored); err != nil {
					return err
				}
				if stored.Preview == nil || stored.Preview.Of != in.Of || stored.Preview.Branch != in.Branch {
					continue
				}
				stored.Preview.Pinned = in.Pinned
				historical.Manifest, err = json.Marshal(stored)
				if err != nil {
					return err
				}
				updates = append(updates, historical)
			}
			// Retention belongs to the preview identity. Update older stored
			// manifests too so rolling back cannot silently undo a pin.
			for _, historical := range updates {
				if err := p.Store.SaveRevision(ctx, historical); err != nil {
					return err
				}
			}
			fmt.Fprintf(out, "%s %s; pinned previews are excluded from preview prune\n", verb, in.App)
			return nil
		},
	}}}, nil
}

// PreviewPrune composes the existing removal plan for a fixed selection. Each
// preview is one retryable step: Remove's steps are idempotent, so recovery can
// repeat a partially finished removal and retain the same aggregate digest.
type PreviewPrune struct{ Remove Remove }
type PreviewPruneInput struct {
	Previews []PreviewTarget `json:"previews"`
	// Explicit allows preview down to remove a pinned preview intentionally.
	Explicit bool `json:"explicit,omitempty"`
}

func (PreviewPrune) Kind() string { return PreviewPruneKind }

func (p PreviewPrune) Plan(ctx context.Context, raw json.RawMessage) (*kernel.Plan, error) {
	var in PreviewPruneInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	if len(in.Previews) == 0 {
		return nil, errors.New("no previews selected")
	}
	plan := &kernel.Plan{Target: "selected previews", Recovery: kernel.Resume}
	seen := map[string]bool{}
	for _, target := range in.Previews {
		if seen[target.App] {
			return nil, fmt.Errorf("preview %s was selected twice", target.App)
		}
		seen[target.App] = true
		remove, err := p.removalPlan(ctx, target, in.Explicit)
		if err != nil {
			return nil, err
		}
		note := "already removed"
		if remove != nil {
			var changes []string
			for _, step := range remove.Steps {
				changes = append(changes, step.Name+": "+step.Change)
			}
			note = strings.Join(changes, "; ")
		}
		plan.Steps = append(plan.Steps, kernel.Step{
			Name:   "remove-" + target.App,
			Change: fmt.Sprintf("remove preview %s of %s at branch %s, revision %s, including its data and secrets", target.App, target.Of, target.Branch, target.Revision),
			Note:   note,
			Apply: func(ctx context.Context, out io.Writer) error {
				remove, err := p.removalPlan(ctx, target, in.Explicit)
				if err != nil {
					return err
				}
				if remove == nil {
					fmt.Fprintf(out, "%s is already removed\n", target.App)
					return nil
				}
				for _, step := range remove.Steps {
					fmt.Fprintf(out, "%s: %s\n", target.App, step.Change)
					if err := step.Apply(ctx, out); err != nil {
						return fmt.Errorf("%s: %s: %w", target.App, step.Name, err)
					}
				}
				return nil
			},
		})
	}
	return plan, nil
}

func (p PreviewPrune) removalPlan(ctx context.Context, target PreviewTarget, explicit bool) (*kernel.Plan, error) {
	rev, _, err := previewRevision(ctx, p.Remove.Store, target, !explicit)
	if errors.Is(err, errPreviewRemoved) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if rev == nil {
		return nil, errors.New("preview revision is missing")
	}
	raw, _ := json.Marshal(RemoveInput{App: target.App, Data: true})
	return p.Remove.Plan(ctx, raw)
}

var errPreviewRemoved = errors.New("preview is already removed")

func previewRevision(ctx context.Context, store *state.Store, target PreviewTarget, refusePinned bool) (*state.Revision, *manifest.Manifest, error) {
	if target.App == "" || target.Revision == "" || target.Of == "" || target.Branch == "" {
		return nil, nil, errors.New("preview selection needs its app, revision, parent and branch")
	}
	revs, err := store.Revisions(ctx, target.App)
	if err != nil {
		return nil, nil, err
	}
	if len(revs) == 0 {
		return nil, nil, errPreviewRemoved
	}
	var found *state.Revision
	for _, rev := range revs {
		if rev.Status == state.RevisionActive && rev.ID != target.Revision {
			return nil, nil, fmt.Errorf("%s was redeployed after it was selected; review cleanup again", target.App)
		}
		if rev.ID == target.Revision {
			copy := rev
			found = &copy
		}
	}
	if found == nil {
		return nil, nil, fmt.Errorf("%s's selected revision is no longer available", target.App)
	}
	var m manifest.Manifest
	if err := json.Unmarshal(found.Manifest, &m); err != nil {
		return nil, nil, err
	}
	if m.Preview == nil || m.Preview.Of != target.Of || m.Preview.Branch != target.Branch || m.App != target.App {
		return nil, nil, fmt.Errorf("%s is not the preview selected for cleanup", target.App)
	}
	if refusePinned && m.Preview.Pinned {
		return nil, nil, fmt.Errorf("%s is pinned; unpin it before pruning", target.App)
	}
	return found, &m, nil
}
