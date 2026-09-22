package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/kylebegeman/bedrock/internal/manifest"
)

func newInit(a *app) *cobra.Command {
	var (
		s     manifest.Scaffold
		kind  string
		dir   string
		force bool
	)
	cmd := &cobra.Command{
		Use:   "init <app>",
		Short: "Write a starting bedrock.yaml for a new app.",
		Long: "Write a starting bedrock.yaml for a new app.\n\n" +
			"The file is commented and ready to deploy once the app itself exists.\n" +
			"Nothing on a machine is touched; this only writes a file.\n\n" +
			"  bedrock init blog --kind static --host blog.example.com\n" +
			"  bedrock init api --host api.example.com --postgres\n" +
			"  bedrock init mailer --kind worker\n" +
			"  bedrock init nightly --kind cron --schedule \"0 4 * * *\"",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s.App = args[0]
			s.Kind = manifest.Kind(kind)
			body, err := s.Render()
			if err != nil {
				return err
			}
			if info, err := os.Stat(dir); os.IsNotExist(err) {
				return fmt.Errorf("%s doesn't exist; make it first, or pass --dir", dir)
			} else if err != nil {
				return err
			} else if !info.IsDir() {
				return fmt.Errorf("%s isn't a directory", dir)
			}
			path := filepath.Join(dir, manifest.FileName)
			if _, err := os.Stat(path); err == nil && !force {
				return fmt.Errorf("%s already exists; pass --force to overwrite it", path)
			} else if err != nil && !os.IsNotExist(err) {
				return err
			}
			if err := os.WriteFile(path, body, 0o644); err != nil {
				return err
			}
			next := s.Next()
			if a.json {
				return json.NewEncoder(a.stdout).Encode(struct {
					Path string   `json:"path"`
					Next []string `json:"next"`
				}{path, next})
			}
			fmt.Fprintf(a.stdout, "Wrote %s\n", path)
			if len(next) > 0 {
				fmt.Fprintln(a.stdout, "\nStill to do:")
				for _, step := range next {
					fmt.Fprintf(a.stdout, "  - %s\n", step)
				}
			}
			fmt.Fprintf(a.stdout, "\nThen: bedrock deploy %s\n", s.App)
			return nil
		},
	}
	cmd.Flags().StringVar(&kind, "kind", string(manifest.Web), "what the app is: web, static, worker or cron")
	cmd.Flags().StringVar(&s.Host, "host", "", "the hostname it answers on, for web and static")
	cmd.Flags().IntVar(&s.Port, "port", 0, fmt.Sprintf("the port a web app listens on (default %d)", manifest.DefaultPort))
	cmd.Flags().StringVar(&s.Schedule, "schedule", "", fmt.Sprintf("when a cron app runs (default %q)", manifest.DefaultSchedule))
	cmd.Flags().BoolVar(&s.Postgres, "postgres", false, "give the app a database, and DATABASE_URL to reach it")
	cmd.Flags().StringVar(&dir, "dir", ".", "the directory to write the manifest into")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing manifest")
	return cmd
}
