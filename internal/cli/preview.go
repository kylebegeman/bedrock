package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	apps "github.com/kylebegeman/helios/internal/app"
	"github.com/kylebegeman/helios/internal/gitdeploy"
	"github.com/kylebegeman/helios/internal/manifest"
	"github.com/kylebegeman/helios/internal/state"
)

func newPreview(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "preview",
		Short: "Run a branch of an app beside it, behind a sign-in.",
		Long: `Run a branch of an app beside it, behind a sign-in.

A preview is an ordinary app with a derived name, its own containers and its
own empty database, deployed from a branch. Pushing the same branch again
updates it rather than making a second one.

  helios preview up https://github.com/you/app --branch new-nav \
      --domain preview.example.com
  helios preview ls
  helios preview down app --branch new-nav
  helios preview pin app --branch new-nav
  helios preview prune --older-than 7d --plan

Every preview route is behind the Core's sign-in and there is no way to turn
that off, so the machine needs the loom integration before any of this works.`,
	}
	cmd.AddCommand(newPreviewUp(a), newPreviewLs(a), newPreviewDown(a), newPreviewPin(a, true), newPreviewPin(a, false), newPreviewPrune(a))
	return cmd
}

// previewUp is one run of preview up: its flags, and the steps from a
// repository to a deployed preview.
type previewUp struct {
	a            *app
	branch       string
	domain       string
	manifestName string
	dnsMode      string
	secrets      bool
	planOnly     bool
	pin          bool
	pinSet       bool
	skipped      []apps.PreviewCheckSkip
}

func newPreviewUp(a *app) *cobra.Command {
	p := &previewUp{a: a}
	cmd := &cobra.Command{
		Use:   "up <repository>",
		Short: "Deploy a branch as a preview of the app in it.",
		Long: "Deploy a branch as a preview of the app in it.\n\n" +
			"The branch's own helios.yaml says which app this is a preview of. The\n" +
			"preview is derived from it: a name with the branch in it, hostnames under\n" +
			"the preview domain, every route behind a sign-in, preserved checks and no backups.\n\n" +
			"The database starts empty and is built by whatever the app runs to migrate\n" +
			"itself. Most branches want a schema rather than somebody's real rows.\n\n" +
			"The preview's hostnames keep their records the way the app's own routes\n" +
			"say. An app whose records are kept by hand has none for a preview, so\n" +
			"either --dns direct makes them, or a wildcard under the preview domain\n" +
			"points at this machine already.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p.pinSet = cmd.Flags().Changed("pin")
			return p.run(cmd.Context(), args[0])
		},
	}
	cmd.Flags().StringVar(&p.branch, "branch", "", "the branch to preview")
	cmd.Flags().StringVar(&p.domain, "domain", "", "the domain previews answer under, such as preview.example.com")
	cmd.Flags().StringVar(&p.manifestName, "manifest", "", "the manifest at the branch's root, for a source that holds several apps")
	cmd.Flags().StringVar(&p.dnsMode, "dns", "", "keep the preview's records this way: direct, proxied or manual (default: as the app's routes say)")
	cmd.Flags().BoolVar(&p.secrets, "secrets", true, "copy the parent's hand-set secrets so the preview can start")
	cmd.Flags().BoolVar(&p.pin, "pin", false, "keep this preview out of preview prune (existing pin is preserved unless explicitly set)")
	a.mutatingFlags(cmd, &p.planOnly)
	return cmd
}

// check refuses what cannot be previewed before anything is fetched, and
// reads --dns.
func (p *previewUp) check(repo string) (mode manifest.DNSMode, override bool, err error) {
	if err := gitdeploy.ValidRepo(repo); err != nil {
		return "", false, err
	}
	if p.branch == "" {
		return "", false, fmt.Errorf("which branch? pass --branch")
	}
	if err := gitdeploy.ValidBranch(p.branch); err != nil {
		return "", false, err
	}
	if p.domain == "" {
		return "", false, fmt.Errorf("where do previews live? pass --domain, such as --domain preview.example.com")
	}
	return previewDNS(p.dnsMode)
}

func (p *previewUp) run(ctx context.Context, repo string) error {
	a := p.a
	mode, override, err := p.check(repo)
	if err != nil {
		return err
	}
	// The tree is fetched under a name of its own and moved beside the
	// app's other sources once the app's name is known. A planned or
	// failed preview leaves nothing behind; a deployed one keeps the
	// newest few, because its revision names it.
	root := filepath.Join(a.stateDir, "builds")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(root, ".preview-")
	if err != nil {
		return err
	}
	deployed := false
	defer func() {
		if !deployed {
			_ = os.RemoveAll(dir)
		}
	}()
	fmt.Fprintf(a.stderr, "fetching %s at %s\n", repo, p.branch)
	if err := shallowClone(ctx, repo, p.branch, dir); err != nil {
		return err
	}
	parent, derived, err := p.derive(ctx, dir, mode, override)
	if err != nil {
		return err
	}
	home := gitdeploy.BuildDir(root, derived.App, "preview")
	if err := os.MkdirAll(filepath.Dir(home), 0o755); err != nil {
		return err
	}
	if err := os.Rename(dir, home); err != nil {
		return err
	}
	dir = home
	name, err := p.writeManifest(dir, derived)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stderr, "%s is a preview of %s at %s\n\n", derived.App, parent.App, hostsOrIts(derived.Hosts()))
	for _, skipped := range p.skipped {
		fmt.Fprintf(a.stderr, "check %s skipped: %s\n", skipped.URL, skipped.Reason)
	}

	in := apps.DeployInput{Source: dir, Manifest: name, Revision: apps.NewRevision(time.Now())}
	if p.secrets {
		// A step of the deploy, so a plan shows it and only an applied
		// plan does it.
		in.SecretsFrom = parent.App
	}
	if err := a.operate(ctx, apps.DeployKind, in, p.planOnly); err != nil {
		return err
	}
	if p.planOnly {
		return nil
	}
	deployed = true
	gitdeploy.PruneBuilds(root, derived.App, 3)
	fmt.Fprintf(a.prose(), "\n%s is up. Remove it with: helios preview down %s\n", derived.App, derived.App)
	return nil
}

// derive reads the branch's app and makes the preview of it.
func (p *previewUp) derive(ctx context.Context, dir string, mode manifest.DNSMode, override bool) (parent, derived *manifest.Manifest, err error) {
	p.manifestName, err = manifest.ResolveFileName(dir, p.manifestName)
	if err != nil {
		return nil, nil, err
	}
	parent, err = manifest.LoadFile(dir, p.manifestName)
	if err != nil {
		return nil, nil, fmt.Errorf("the branch has no app to preview: %w", err)
	}
	if parent.Preview != nil {
		return nil, nil, fmt.Errorf("%s is already a preview of %s; previews are not previewed", parent.App, parent.Preview.Of)
	}
	derived, p.skipped, err = apps.PreviewOfWithReport(parent, p.branch, p.domain)
	if err != nil {
		return nil, nil, err
	}
	if override {
		apps.SetRouteDNS(derived, mode)
	}
	derived.Preview.Pinned = p.pin
	{
		store, openErr := p.a.openState()
		if openErr != nil {
			return nil, nil, openErr
		}
		defer store.Close()
		current, readErr := store.RevisionWithStatus(ctx, derived.App, state.RevisionActive)
		if readErr != nil {
			return nil, nil, readErr
		}
		if current != nil {
			var previous manifest.Manifest
			if err := json.Unmarshal(current.Manifest, &previous); err != nil {
				return nil, nil, err
			}
			if previous.Preview == nil || previous.Preview.Of != parent.App || previous.Preview.Branch != p.branch {
				return nil, nil, fmt.Errorf("%s already belongs to another app or branch; choose a different branch name", derived.App)
			}
			if !p.pinSet {
				derived.Preview.Pinned = previous.Preview.Pinned
			}
		}
	}
	return parent, derived, nil
}

// writeManifest writes the derived manifest into the tree and returns its
// name. It goes under its own name, beside the branch's real manifest
// rather than over it: the tree is a clone, but a manifest that silently
// replaced the app's own would be a trap if it ever were not.
func (p *previewUp) writeManifest(dir string, derived *manifest.Manifest) (string, error) {
	body, err := yaml.Marshal(derived)
	if err != nil {
		return "", err
	}
	name := previewManifestName(p.manifestName)
	if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
		return "", err
	}
	// Parse it back from where it will actually be read.
	if _, err := manifest.LoadFile(dir, name); err != nil {
		return "", fmt.Errorf("the derived manifest does not load, which is a bug in helios: %w", err)
	}
	return name, nil
}

// previewDNS reads the --dns flag: the mode, and whether one was given.
func previewDNS(flag string) (manifest.DNSMode, bool, error) {
	switch flag {
	case "":
		return "", false, nil
	case "manual":
		return manifest.DNSManual, true, nil
	case "direct":
		return manifest.DNSDirect, true, nil
	case "proxied":
		return manifest.DNSProxied, true, nil
	}
	return "", false, fmt.Errorf("--dns %q isn't direct, proxied or manual", flag)
}

// previewManifestName is where a preview's derived manifest is written,
// beside the one it was derived from: helios.preview.yaml for
// helios.yaml, worker.preview.yml for worker.yml.
func previewManifestName(name string) string {
	ext := filepath.Ext(name)
	return strings.TrimSuffix(name, ext) + ".preview" + ext
}

func newPreviewLs(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the previews running on this machine.",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := a.openState()
			if err != nil {
				return err
			}
			defer store.Close()
			revs, err := store.ActiveRevisions(cmd.Context())
			if err != nil {
				return err
			}
			type row struct {
				Preview    string    `json:"preview"`
				Of         string    `json:"of"`
				Branch     string    `json:"branch"`
				Hosts      []string  `json:"hosts,omitempty"`
				DeployedAt time.Time `json:"deployed_at"`
				Pinned     bool      `json:"pinned"`
			}
			rows := []row{}
			for _, rev := range revs {
				// Stored revisions keep the manifest as JSON; its preview
				// block is what says an app is one.
				var m manifest.Manifest
				if err := json.Unmarshal(rev.Manifest, &m); err != nil || m.Preview == nil {
					continue
				}
				rows = append(rows, row{rev.App, m.Preview.Of, m.Preview.Branch, m.Hosts(), rev.CreatedAt, m.Preview.Pinned})
			}
			sort.Slice(rows, func(i, j int) bool { return rows[i].Preview < rows[j].Preview })
			if a.json {
				return json.NewEncoder(a.stdout).Encode(rows)
			}
			if len(rows) == 0 {
				fmt.Fprintln(a.stdout, "no previews")
				return nil
			}
			w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "PREVIEW\tOF\tBRANCH\tHOSTS\tDEPLOYED\tPINNED")
			for _, r := range rows {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s ago\t%t\n", r.Preview, r.Of, r.Branch, strings.Join(r.Hosts, ", "), time.Since(r.DeployedAt).Round(time.Minute), r.Pinned)
			}
			return w.Flush()
		},
	}
}

// selectedPreview resolves metadata, never a guessed name prefix.
func selectedPreview(ctx context.Context, a *app, name, branch string) (apps.PreviewTarget, error) {
	store, err := a.openState()
	if err != nil {
		return apps.PreviewTarget{}, err
	}
	defer store.Close()
	rev, err := apps.ResolvePreview(ctx, store, name, branch)
	if err != nil {
		return apps.PreviewTarget{}, err
	}
	return apps.PreviewTargetOf(rev)
}

func newPreviewDown(a *app) *cobra.Command {
	var branch string
	var planOnly bool
	cmd := &cobra.Command{
		Use:   "down <preview-or-parent>",
		Short: "Remove a preview, including its data and secrets.",
		Long:  "Remove a preview, including its data and secrets.\n\nGive the preview's name, or its parent app with --branch. Pinned previews can\nbe removed explicitly here; preview prune leaves them alone.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := selectedPreview(cmd.Context(), a, args[0], branch)
			if err != nil {
				return err
			}
			return a.operate(cmd.Context(), apps.PreviewPruneKind, apps.PreviewPruneInput{Previews: []apps.PreviewTarget{target}, Explicit: true}, planOnly)
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "the exact branch when naming the parent app")
	a.mutatingFlags(cmd, &planOnly)
	return cmd
}

func newPreviewPin(a *app, pinned bool) *cobra.Command {
	verb := "pin"
	short := "Keep a preview out of preview prune."
	if !pinned {
		verb, short = "unpin", "Allow preview prune to remove this preview when old enough."
	}
	var branch string
	var planOnly bool
	cmd := &cobra.Command{
		Use: verb + " <preview-or-parent>", Short: short, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := selectedPreview(cmd.Context(), a, args[0], branch)
			if err != nil {
				return err
			}
			return a.operate(cmd.Context(), apps.PreviewPinKind, apps.PreviewPinInput{PreviewTarget: target, Pinned: pinned}, planOnly)
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "the exact branch when naming the parent app")
	a.mutatingFlags(cmd, &planOnly)
	return cmd
}

func newPreviewPrune(a *app) *cobra.Command {
	var age string
	var planOnly bool
	cmd := &cobra.Command{
		Use: "prune [parent-app]", Short: "Remove old, unpinned previews with one reviewable plan.",
		Long: "Remove old, unpinned previews with one reviewable plan.\n\nAge is measured from the latest deploy, not traffic or branch activity. Every\nselected preview's data and secrets are removed using the ordinary remove\nplan. Production apps and pinned previews are excluded.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			duration, err := apps.ParsePreviewAge(age)
			if err != nil {
				return err
			}
			store, err := a.openState()
			if err != nil {
				return err
			}
			defer store.Close()
			selected, err := apps.PrunablePreviews(cmd.Context(), store, optional(args, 0), time.Now().UTC().Add(-duration))
			if err != nil {
				return err
			}
			if len(selected) == 0 {
				if a.digest != "" {
					return fmt.Errorf("no previews match the approved cleanup now; review the plan again")
				}
				if a.json {
					return json.NewEncoder(a.stdout).Encode([]apps.PreviewTarget{})
				}
				fmt.Fprintln(a.stdout, "no unpinned previews match")
				return nil
			}
			return a.operate(cmd.Context(), apps.PreviewPruneKind, apps.PreviewPruneInput{Previews: selected}, planOnly)
		},
	}
	cmd.Flags().StringVar(&age, "older-than", "7d", "time since latest deploy, such as 48h or 7d")
	a.mutatingFlags(cmd, &planOnly)
	return cmd
}
