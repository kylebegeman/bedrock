package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kylebegeman/quark/internal/docker"
	"github.com/kylebegeman/quark/internal/manifest"
	"github.com/kylebegeman/quark/internal/secrets"
)

// Names the data services use.
const (
	postgresWorkload = "postgres"
	// DatabaseURLName is the secret every workload of an app with a
	// database receives.
	DatabaseURLName = "DATABASE_URL"
	// postgresPasswordName is the internal secret the database container
	// itself is started with.
	postgresPasswordName = "QUARK_POSTGRES_PASSWORD"
)

// PostgresContainer names an app's database container. It carries no
// revision: the database outlives every revision.
func PostgresContainer(app string) string { return "quark-" + app + "-" + postgresWorkload }

// postgresName turns an app name into a database and role name.
func postgresName(app string) string { return strings.ReplaceAll(app, "-", "_") }

// Restore says what to load into an app's data before its first start.
type Restore struct {
	// Postgres is a pg_dump custom-format file on this machine.
	Postgres string `json:"postgres,omitempty"`
	// Volumes maps a volume name to a .tar.gz on this machine.
	Volumes map[string]string `json:"volumes,omitempty"`
}

// ensureData brings up the app's volumes and database, restoring into
// them when asked and they are empty.
func ensureData(ctx context.Context, e *docker.Engine, store *secrets.Store, m *manifest.Manifest, restore Restore, out io.Writer) error {
	if m.Data == nil {
		return nil
	}
	for name := range m.Data.Volumes {
		vol := docker.VolumeName(m.App, name)
		if err := e.EnsureVolume(ctx, vol); err != nil {
			return err
		}
		tarball, wanted := restore.Volumes[name]
		if !wanted {
			continue
		}
		empty, err := e.VolumeEmpty(ctx, vol)
		if err != nil {
			return err
		}
		if !empty {
			return fmt.Errorf("volume %s already has files; a restore is for an empty volume", name)
		}
		if err := e.RestoreVolume(ctx, vol, tarball); err != nil {
			return err
		}
		fmt.Fprintf(out, "volume %s restored from %s\n", name, tarball)
	}
	for name := range restore.Volumes {
		if _, declared := m.Data.Volumes[name]; !declared {
			return fmt.Errorf("no volume named %s in the manifest", name)
		}
	}
	if version := m.PostgresVersion(); version != "" {
		if err := ensurePostgres(ctx, e, store, m, version, restore.Postgres, out); err != nil {
			return err
		}
	} else if restore.Postgres != "" {
		return errors.New("the manifest declares no database to restore into")
	}
	return nil
}

func ensurePostgres(ctx context.Context, e *docker.Engine, store *secrets.Store, m *manifest.Manifest, version, dump string, out io.Writer) error {
	values, _, err := store.LoadCurrent(m.App)
	if err != nil {
		return err
	}
	dbName := postgresName(m.App)
	password := values[postgresPasswordName]
	if password == "" {
		var b [24]byte
		if _, err := rand.Read(b[:]); err != nil {
			return err
		}
		password = hex.EncodeToString(b[:])
		if _, err := store.Set(m.App, postgresPasswordName, password); err != nil {
			return err
		}
		url := fmt.Sprintf("postgres://%s:%s@%s:5432/%s?sslmode=disable", dbName, password, PostgresContainer(m.App), dbName)
		if _, err := store.Set(m.App, DatabaseURLName, url); err != nil {
			return err
		}
		fmt.Fprintf(out, "database credentials made; %s is in the app's secrets\n", DatabaseURLName)
	}
	container := PostgresContainer(m.App)
	image := "postgres:" + version + "-alpine"
	if !e.HasImage(ctx, image) {
		if err := e.Pull(ctx, image, out); err != nil {
			return err
		}
	}
	volume := docker.VolumeName(m.App, postgresWorkload)
	if err := e.EnsureVolume(ctx, volume); err != nil {
		return err
	}
	err = e.Run(ctx, docker.Spec{
		Name:     container,
		Image:    image,
		Env:      []string{"POSTGRES_USER=" + dbName, "POSTGRES_DB=" + dbName, "POSTGRES_PASSWORD=" + password, "PGDATA=/var/lib/postgresql/data/pgdata"},
		Labels:   map[string]string{docker.LabelApp: m.App, docker.LabelWorkload: postgresWorkload},
		Networks: []string{docker.AppNetwork(m.App)},
		Mounts:   []string{volume + ":/var/lib/postgresql/data"},
		Restart:  true,
	})
	if err != nil {
		return err
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		if _, err := e.Exec(ctx, container, nil, "pg_isready", "-U", dbName, "-d", dbName); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("postgres %s didn't become ready in 90s; see docker logs %s", version, container)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	fmt.Fprintf(out, "postgres %s ready as %s\n", version, container)
	if dump == "" {
		return nil
	}
	tables, err := e.Exec(ctx, container, nil, "psql", "-U", dbName, "-d", dbName, "-tAc", "select count(*) from pg_tables where schemaname = 'public'")
	if err != nil {
		return err
	}
	if n, _ := strconv.Atoi(strings.TrimSpace(tables)); n > 0 {
		return fmt.Errorf("the database already has %d tables; a restore is for an empty database", n)
	}
	f, err := os.Open(dump)
	if err != nil {
		return err
	}
	defer f.Close()
	if output, err := e.Exec(ctx, container, f, "pg_restore", "-U", dbName, "-d", dbName, "--no-owner", "--no-privileges"); err != nil {
		return fmt.Errorf("pg_restore: %w\n%s", err, tail(output, 5))
	}
	tables, _ = e.Exec(ctx, container, nil, "psql", "-U", dbName, "-d", dbName, "-tAc", "select count(*) from pg_tables where schemaname = 'public'")
	fmt.Fprintf(out, "database restored from %s: %s tables\n", dump, strings.TrimSpace(tables))
	return nil
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// fixVolumeOwners makes restored files belong to the user the mounting
// workload's image runs as.
func fixVolumeOwners(ctx context.Context, e *docker.Engine, m *manifest.Manifest, images map[string]string, restored map[string]string, out io.Writer) error {
	for volName := range restored {
		for _, wl := range m.WorkloadNames() {
			w := m.Workloads[wl]
			for _, mt := range w.Mounts {
				if mt.Volume != volName {
					continue
				}
				image := images[wl]
				if image == "" {
					continue
				}
				user, err := e.ImageUser(ctx, image)
				if err != nil {
					return err
				}
				if user == "" || user == "root" || user == "0" {
					continue
				}
				if err := e.ChownVolume(ctx, docker.VolumeName(m.App, volName), image, user, mt.Path); err != nil {
					return err
				}
				fmt.Fprintf(out, "volume %s now belongs to %s, as %s expects\n", volName, user, wl)
				break
			}
		}
	}
	return nil
}
