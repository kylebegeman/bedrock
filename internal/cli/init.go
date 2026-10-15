package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/kylebegeman/helios/internal/manifest"
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
		Short: "Write a starting helios.yaml for a new app.",
		Long: "Write a starting helios.yaml for a new app.\n\n" +
			"The file is commented and ready to deploy once the app itself exists.\n" +
			"Nothing on a machine is touched; this only writes a file.\n\n" +
			"  helios init blog --kind static --host blog.example.com\n" +
			"  helios init api --host api.example.com --postgres\n" +
			"  helios init mailer --kind worker\n" +
			"  helios init nightly --kind cron --schedule \"0 4 * * *\"",
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
			name, err := manifest.ResolveFileName(dir, "")
			if err != nil {
				return err
			}
			path := filepath.Join(dir, name)
			if info, err := os.Lstat(path); err == nil {
				if !info.Mode().IsRegular() {
					return fmt.Errorf("%s must be a plain file, not a link or directory", path)
				}
				if !force {
					return fmt.Errorf("%s already exists; pass --force to overwrite it", path)
				}
			} else if !os.IsNotExist(err) {
				return err
			}
			if err := writeInitManifest(path, body, force); err != nil {
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
			fmt.Fprintf(a.stdout, "\nThen: helios deploy %s\n", dir)
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

// An unforced init creates exclusively. A forced replacement uses rename so
// even a link introduced after validation cannot redirect the write elsewhere.
func writeInitManifest(path string, body []byte, force bool) error {
	if !force {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		_, err = f.Write(body)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".helios-init-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
