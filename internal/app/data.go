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
	volume := docker.VolumeName(m.App, postgresWorkload)
	if err := startPostgres(ctx, e, container, image, dbName, password, volume, docker.AppNetwork(m.App), out); err != nil {
		return err
	}
	fmt.Fprintf(out, "postgres %s ready as %s\n", version, container)
	if dump == "" {
		return nil
	}
	tables, err := restoreDump(ctx, e, container, dbName, dump)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "database restored from %s: %s tables\n", dump, tables)
	return nil
}

// startPostgres runs a database container on a volume and network and
// waits until it accepts connections.
func startPostgres(ctx context.Context, e *docker.Engine, container, image, dbName, password, volume, network string, out io.Writer) error {
	if !e.HasImage(ctx, image) {
		if err := e.Pull(ctx, image, out); err != nil {
			return err
		}
	}
	if err := e.EnsureVolume(ctx, volume); err != nil {
		return err
	}
	err := e.Run(ctx, docker.Spec{
		Name:     container,
		Image:    image,
		Env:      []string{"POSTGRES_USER=" + dbName, "POSTGRES_DB=" + dbName, "POSTGRES_PASSWORD=" + password, "PGDATA=/var/lib/postgresql/data/pgdata"},
		Labels:   map[string]string{docker.LabelApp: appOf(container), docker.LabelWorkload: postgresWorkload},
		Networks: []string{network},
		Mounts:   []string{volume + ":/var/lib/postgresql/data"},
		Restart:  true,
	})
	if err != nil {
		return err
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		if _, err := e.Exec(ctx, container, nil, "pg_isready", "-U", dbName, "-d", dbName); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("postgres didn't become ready in 90s; see docker logs %s", container)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// appOf reads the app name back out of a quark container name.
func appOf(container string) string {
	name := strings.TrimPrefix(container, "quark-")
	for _, suffix := range []string{"-drill-postgres", "-postgres"} {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix)
		}
	}
	return name
}

// restoreDump loads a pg_dump custom-format file into a database that
// has no tables yet, and returns how many it has afterwards.
func restoreDump(ctx context.Context, e *docker.Engine, container, dbName, dump string) (string, error) {
	tables, err := tableCount(ctx, e, container, dbName)
	if err != nil {
		return "", err
	}
	if tables > 0 {
		return "", fmt.Errorf("the database already has %d tables; a restore is for an empty database", tables)
	}
	f, err := os.Open(dump)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if output, err := e.Exec(ctx, container, f, "pg_restore", "-U", dbName, "-d", dbName, "--no-owner", "--no-privileges"); err != nil {
		return "", fmt.Errorf("pg_restore: %w\n%s", err, tail(output, 5))
	}
	tables, err = tableCount(ctx, e, container, dbName)
	if err != nil {
		return "", err
	}
	return strconv.Itoa(tables), nil
}

// tableCount counts the public tables in a database.
func tableCount(ctx context.Context, e *docker.Engine, container, dbName string) (int, error) {
	out, err := scalar(ctx, e, container, dbName, "select count(*) from pg_tables where schemaname = 'public'")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(out)
}

// scalar runs a query that returns one value.
func scalar(ctx context.Context, e *docker.Engine, container, dbName, query string) (string, error) {
	out, err := e.Exec(ctx, container, nil, "psql", "-U", dbName, "-d", dbName, "-tAc", query)
	if err != nil {
		return "", fmt.Errorf("query: %w", err)
	}
	return strings.TrimSpace(out), nil
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
