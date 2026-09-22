package cli

import (
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

	apps "github.com/kylebegeman/bedrock/internal/app"
	"github.com/kylebegeman/bedrock/internal/gitdeploy"
	"github.com/kylebegeman/bedrock/internal/manifest"
)

func newPreview(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "preview",
		Short: "Run a branch of an app beside it, behind a sign-in.",
		Long: `Run a branch of an app beside it, behind a sign-in.

A preview is an ordinary app with a derived name, its own containers and its
own empty database, deployed from a branch. Pushing the same branch again
updates it rather than making a second one.

  bedrock preview up https://github.com/you/app --branch new-nav \
      --domain preview.example.com
  bedrock preview ls
  bedrock remove app-pr-new-nav --data

Every preview route is behind the Core's sign-in and there is no way to turn
that off, so the machine needs the loom integration before any of this works.`,
	}
	cmd.AddCommand(newPreviewUp(a), newPreviewLs(a))
	return cmd
}

func newPreviewUp(a *app) *cobra.Command {
	var (
		branch   string
		domain   string
		secrets  bool
		planOnly bool
	)
	cmd := &cobra.Command{
		Use:   "up <repository>",
		Short: "Deploy a branch as a preview of the app in it.",
		Long: "Deploy a branch as a preview of the app in it.\n\n" +
			"The branch's own bedrock.yaml says which app this is a preview of. The\n" +
			"preview is derived from it: a name with the branch in it, hostnames under\n" +
			"the preview domain, every route behind a sign-in, no checks and no backups.\n\n" +
			"The database starts empty and is built by whatever the app runs to migrate\n" +
			"itself. Most branches want a schema rather than somebody's real rows.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo := args[0]
			if err := gitdeploy.ValidRepo(repo); err != nil {
				return err
			}
			if branch == "" {
				return fmt.Errorf("which branch? pass --branch")
			}
			if domain == "" {
				return fmt.Errorf("where do previews live? pass --domain, such as --domain preview.example.com")
			}
			dir := filepath.Join(a.stateDir, "previews", fmt.Sprintf("%s-%d", apps.DNSLabel(branch), time.Now().UnixMilli()))
			if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
				return err
			}
			if planOnly {
				defer os.RemoveAll(dir)
			}
			fmt.Fprintf(a.stderr, "fetching %s at %s\n", repo, branch)
			if err := shallowClone(cmd.Context(), repo, branch, dir); err != nil {
				return err
			}
			parent, err := manifest.Load(dir)
			if err != nil {
				return fmt.Errorf("the branch has no app to preview: %w", err)
			}
			if _, isPreview := apps.IsPreview(parent.App); isPreview {
				return fmt.Errorf("%s is already a preview; previews are not previewed", parent.App)
			}
			derived, err := apps.PreviewOf(parent, branch, domain)
			if err != nil {
				return err
			}
			body, err := yaml.Marshal(derived)
			if err != nil {
				return err
			}
			// Written under its own name, beside the branch's real manifest
			// rather than over it: the tree is a clone, but a manifest that
			// silently replaced the app's own would be a trap if it ever
			// were not.
			name := "bedrock.preview.yaml"
			if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
				return err
			}
			// Parse it back from where it will actually be read.
			if _, err := manifest.LoadFile(dir, name); err != nil {
				return fmt.Errorf("the derived manifest does not load, which is a bug in bedrock: %w", err)
			}
			fmt.Fprintf(a.stderr, "%s is a preview of %s at %s\n", derived.App, parent.App, hostsOrIts(apps.Hosts(derived)))

			if secrets {
				copied, err := a.secretsStore().CopyAll(parent.App, derived.App)
				if err != nil {
					return fmt.Errorf("copying %s's secrets to the preview: %w", parent.App, err)
				}
				if len(copied) > 0 {
					fmt.Fprintf(a.stderr, "copied %d secret(s) from %s\n", len(copied), parent.App)
				}
			}
			fmt.Fprintln(a.stderr)

			in := apps.DeployInput{Source: dir, Manifest: name, Revision: time.Now().UTC().Format("20060102-150405")}
			if err := a.operate(cmd.Context(), apps.DeployKind, in, planOnly); err != nil {
				return err
			}
			if planOnly {
				return nil
			}
			fmt.Fprintf(a.stdout, "\n%s is up. Remove it with: bedrock remove %s --data\n", derived.App, derived.App)
			return nil
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "the branch to preview")
	cmd.Flags().StringVar(&domain, "domain", "", "the domain previews answer under, such as preview.example.com")
	cmd.Flags().BoolVar(&secrets, "secrets", true, "copy the parent's hand-set secrets so the preview can start")
	a.mutatingFlags(cmd, &planOnly)
	return cmd
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
				app, parent, hosts string
				since              time.Duration
			}
			var rows []row
			for _, rev := range revs {
				parent, ok := apps.IsPreview(rev.App)
				if !ok {
					continue
				}
				// Stored revisions keep the manifest as JSON.
				var hosts []string
				var m manifest.Manifest
				if err := json.Unmarshal(rev.Manifest, &m); err == nil {
					hosts = apps.Hosts(&m)
				}
				rows = append(rows, row{rev.App, parent, strings.Join(hosts, ", "), time.Since(rev.CreatedAt)})
			}
			sort.Slice(rows, func(i, j int) bool { return rows[i].app < rows[j].app })
			if len(rows) == 0 {
				fmt.Fprintln(a.stdout, "no previews")
				return nil
			}
			w := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "PREVIEW\tOF\tHOSTS\tDEPLOYED")
			for _, r := range rows {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s ago\n", r.app, r.parent, r.hosts, r.since.Round(time.Minute))
			}
			return w.Flush()
		},
	}
}
