package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/kylebegeman/bedrock/internal/docker"
	"github.com/kylebegeman/bedrock/internal/kernel"
	"github.com/kylebegeman/bedrock/internal/state"
)

// GCKind removes what bedrock created and no longer needs: containers and
// images of revisions that are neither active nor kept for rollback, and
// the build cache. It never touches anything without bedrock's label.
const GCKind = "app.gc"

// DefaultBuildCacheMax is how much build cache a machine keeps.
//
// Build cache is a speed optimisation, not data: losing it costs one slow
// build and nothing else. A machine iterating on a large source tree can
// produce tens of gigabytes of it in a day, so the ceiling is deliberately
// far below what an unbounded cache reaches. Raise it with --keep-cache on
// a machine that rebuilds something huge and has the disk to spare.
const DefaultBuildCacheMax = 8 << 30 // 8 GiB

// GC is the Definition for GCKind.
type GC struct {
	Store *state.Store
}

// GCInput configures a collection.
type GCInput struct {
	// KeepBuildCache leaves Docker's build cache alone.
	KeepBuildCache bool `json:"keep_build_cache,omitempty"`

	// BuildCacheMax is how many bytes of build cache to keep. Zero means
	// DefaultBuildCacheMax; a negative value keeps no ceiling at all.
	BuildCacheMax int64 `json:"build_cache_max,omitempty"`
}

// Kind implements kernel.Definition.
func (GC) Kind() string { return GCKind }

// Plan implements kernel.Definition.
func (g GC) Plan(ctx context.Context, raw json.RawMessage) (*kernel.Plan, error) {
	var in GCInput
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, fmt.Errorf("gc input: %w", err)
		}
	}
	e, err := docker.Connect(ctx)
	if err != nil {
		return nil, err
	}
	defer e.Close()
	keep, err := keptRevisions(ctx, g.Store)
	if err != nil {
		return nil, err
	}
	containersNow, err := staleContainers(ctx, e, keep)
	if err != nil {
		return nil, err
	}
	imagesNow, err := staleImages(ctx, e, keep)
	if err != nil {
		return nil, err
	}

	plan := &kernel.Plan{Target: "this machine", Recovery: kernel.Resume}
	plan.Steps = append(plan.Steps, kernel.Step{
		Name: "containers", Change: "remove containers of revisions that are neither active nor kept for rollback",
		Note: countNote(len(containersNow), "container"),
		Apply: func(ctx context.Context, out io.Writer) error {
			e, err := docker.Connect(ctx)
			if err != nil {
				return err
			}
			defer e.Close()
			keep, err := keptRevisions(ctx, g.Store)
			if err != nil {
				return err
			}
			stale, err := staleContainers(ctx, e, keep)
			if err != nil {
				return err
			}
			for _, c := range stale {
				if err := e.Remove(ctx, c.Name, 5*time.Second); err != nil {
					return err
				}
				fmt.Fprintf(out, "removed %s\n", c.Name)
			}
			return nil
		},
	})
	plan.Steps = append(plan.Steps, kernel.Step{
		Name: "images", Change: "remove images of revisions that are neither active nor kept for rollback",
		Note: countNote(len(imagesNow), "image"),
		Apply: func(ctx context.Context, out io.Writer) error {
			e, err := docker.Connect(ctx)
			if err != nil {
				return err
			}
			defer e.Close()
			keep, err := keptRevisions(ctx, g.Store)
			if err != nil {
				return err
			}
			stale, err := staleImages(ctx, e, keep)
			if err != nil {
				return err
			}
			var freed int64
			for _, img := range stale {
				if err := e.RemoveImage(ctx, img.ID); err != nil {
					return err
				}
				freed += img.Size
				fmt.Fprintf(out, "removed %s\n", img.Describe())
			}
			if freed > 0 {
				fmt.Fprintf(out, "freed %s\n", megabytes(freed))
			}
			return nil
		},
	})
	if !in.KeepBuildCache {
		keep := in.BuildCacheMax
		if keep == 0 {
			keep = DefaultBuildCacheMax
		}
		if keep < 0 {
			keep = 0
		}
		change := "drop build cache older than a day"
		if keep > 0 {
			change += ", then keep at most " + megabytes(keep)
		}
		plan.Steps = append(plan.Steps, kernel.Step{
			Name: "build-cache", Change: change,
			Apply: func(ctx context.Context, out io.Writer) error {
				freed, err := docker.PruneBuildCache(ctx, 24*time.Hour, keep)
				if freed > 0 {
					fmt.Fprintf(out, "reclaimed %s of build cache\n", megabytes(freed))
				}
				return err
			},
		})
	}
	plan.Steps = append(plan.Steps, kernel.Step{
		Name: "forget", Change: "forget retired revisions older than the last five",
		Apply: func(ctx context.Context, out io.Writer) error {
			apps, err := g.Store.Apps(ctx)
			if err != nil {
				return err
			}
			for _, a := range apps {
				revs, err := g.Store.Revisions(ctx, a.Name)
				if err != nil {
					return err
				}
				var old []state.Revision
				for _, r := range revs {
					if r.Status == state.RevisionRetired || r.Status == state.RevisionFailed {
						old = append(old, r)
					}
				}
				for i, r := range old {
					if i < 5 {
						continue
					}
					if err := g.Store.ForgetRevision(ctx, r.App, r.ID); err != nil {
						return err
					}
					fmt.Fprintf(out, "forgot %s %s\n", r.App, r.ID)
				}
			}
			return nil
		},
	})
	return plan, nil
}

// keptRevisions returns "app/revision" for every active or previous revision.
func keptRevisions(ctx context.Context, store *state.Store) (map[string]bool, error) {
	keep := map[string]bool{}
	apps, err := store.Apps(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range apps {
		revs, err := store.Revisions(ctx, a.Name)
		if err != nil {
			return nil, err
		}
		for _, r := range revs {
			if r.Status == state.RevisionActive || r.Status == state.RevisionPrevious {
				keep[r.App+"/"+r.ID] = true
			}
		}
	}
	return keep, nil
}

// staleContainers are bedrock's containers that belong to a revision nobody
// keeps. Containers without a revision label (the edge, the registry) stay.
func staleContainers(ctx context.Context, e *docker.Engine, keep map[string]bool) ([]docker.Info, error) {
	owned, err := e.Owned(ctx)
	if err != nil {
		return nil, err
	}
	var stale []docker.Info
	for _, c := range owned {
		rev := c.Labels[docker.LabelRevision]
		if rev == "" || keep[c.Labels[docker.LabelApp]+"/"+rev] {
			continue
		}
		stale = append(stale, c)
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].Name < stale[j].Name })
	return stale, nil
}

// staleImages are images bedrock built for revisions nobody keeps.
func staleImages(ctx context.Context, e *docker.Engine, keep map[string]bool) ([]docker.Image, error) {
	images, err := e.OwnedImages(ctx)
	if err != nil {
		return nil, err
	}
	var stale []docker.Image
	for _, img := range images {
		rev := img.Labels[docker.LabelRevision]
		if rev == "" || keep[img.Labels[docker.LabelApp]+"/"+rev] {
			continue
		}
		stale = append(stale, img)
	}
	sort.Slice(stale, func(i, j int) bool { return stale[i].Describe() < stale[j].Describe() })
	return stale, nil
}

func countNote(n int, what string) string {
	switch n {
	case 0:
		return "nothing to remove"
	case 1:
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

func megabytes(b int64) string {
	if b >= 1<<30 {
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	}
	return fmt.Sprintf("%d MB", b>>20)
}
