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
		branch       string
		domain       string
		manifestName string
		dnsMode      string
		secrets      bool
		planOnly     bool
	)
	cmd := &cobra.Command{
		Use:   "up <repository>",
		Short: "Deploy a branch as a preview of the app in it.",
		Long: "Deploy a branch as a preview of the app in it.\n\n" +
			"The branch's own bedrock.yaml says which app this is a preview of. The\n" +
			"preview is derived from it: a name with the branch in it, hostnames under\n" +
			"the preview domain, every route behind a sign-in, no checks and no backups.\n\n" +
			"The database starts empty and is built by whatever the app runs to migrate\n" +
			"itself. Most branches want a schema rather than somebody's real rows.\n\n" +
			"The preview's hostnames keep their records the way the app's own routes\n" +
			"say. An app whose records are kept by hand has none for a preview, so\n" +
			"either --dns direct makes them, or a wildcard under the preview domain\n" +
			"points at this machine already.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo := args[0]
			if err := gitdeploy.ValidRepo(repo); err != nil {
				return err
			}
			if branch == "" {
				return fmt.Errorf("which branch? pass --branch")
			}
			if err := gitdeploy.ValidBranch(branch); err != nil {
				return err
			}
			if domain == "" {
				return fmt.Errorf("where do previews live? pass --domain, such as --domain preview.example.com")
			}
			mode, override, err := previewDNS(dnsMode)
			if err != nil {
				return err
			}
			// The tree is fetched under a name of its own and moved beside
			// the app's other sources once the app's name is known. A
			// planned or failed preview leaves nothing behind; a deployed
			// one keeps the newest few, because its revision names it.
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
			fmt.Fprintf(a.stderr, "fetching %s at %s\n", repo, branch)
			if err := shallowClone(cmd.Context(), repo, branch, dir); err != nil {
				return err
			}
			parent, err := manifest.LoadFile(dir, manifestName)
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
			if override {
				apps.SetRouteDNS(derived, mode)
			}
			home := gitdeploy.BuildDir(root, derived.App, "preview")
			if err := os.MkdirAll(filepath.Dir(home), 0o755); err != nil {
				return err
			}
			if err := os.Rename(dir, home); err != nil {
				return err
			}
			dir = home
			body, err := yaml.Marshal(derived)
			if err != nil {
				return err
			}
			// Written under its own name, beside the branch's real manifest
			// rather than over it: the tree is a clone, but a manifest that
			// silently replaced the app's own would be a trap if it ever
			// were not.
			name := previewManifestName(manifestName)
			if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
				return err
			}
			// Parse it back from where it will actually be read.
			if _, err := manifest.LoadFile(dir, name); err != nil {
				return fmt.Errorf("the derived manifest does not load, which is a bug in bedrock: %w", err)
			}
			fmt.Fprintf(a.stderr, "%s is a preview of %s at %s\n\n", derived.App, parent.App, hostsOrIts(derived.Hosts()))

			in := apps.DeployInput{Source: dir, Manifest: name, Revision: apps.NewRevision(time.Now())}
			if secrets {
				// A step of the deploy, so a plan shows it and only an
				// applied plan does it.
				in.SecretsFrom = parent.App
			}
			if err := a.operate(cmd.Context(), apps.DeployKind, in, planOnly); err != nil {
				return err
			}
			if planOnly {
				return nil
			}
			deployed = true
			gitdeploy.PruneBuilds(root, derived.App, 3)
			fmt.Fprintf(a.prose(), "\n%s is up. Remove it with: bedrock remove %s --data\n", derived.App, derived.App)
			return nil
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "the branch to preview")
	cmd.Flags().StringVar(&domain, "domain", "", "the domain previews answer under, such as preview.example.com")
	cmd.Flags().StringVar(&manifestName, "manifest", manifest.FileName, "the manifest at the branch's root, for a source that holds several apps")
	cmd.Flags().StringVar(&dnsMode, "dns", "", "keep the preview's records this way: direct, proxied or manual (default: as the app's routes say)")
	cmd.Flags().BoolVar(&secrets, "secrets", true, "copy the parent's hand-set secrets so the preview can start")
	a.mutatingFlags(cmd, &planOnly)
	return cmd
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
// beside the one it was derived from: bedrock.preview.yaml for
// bedrock.yaml, worker.preview.yml for worker.yml.
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
				Hosts      []string  `json:"hosts,omitempty"`
				DeployedAt time.Time `json:"deployed_at"`
			}
			rows := []row{}
			for _, rev := range revs {
				parent, ok := apps.IsPreview(rev.App)
				if !ok {
					continue
				}
				// Stored revisions keep the manifest as JSON.
				var hosts []string
				var m manifest.Manifest
				if err := json.Unmarshal(rev.Manifest, &m); err == nil {
					hosts = m.Hosts()
				}
				rows = append(rows, row{rev.App, parent, hosts, rev.CreatedAt})
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
			fmt.Fprintln(w, "PREVIEW\tOF\tHOSTS\tDEPLOYED")
			for _, r := range rows {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s ago\n", r.Preview, r.Of, strings.Join(r.Hosts, ", "), time.Since(r.DeployedAt).Round(time.Minute))
			}
			return w.Flush()
		},
	}
}
