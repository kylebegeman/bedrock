package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/bedrock/internal/docker"
	"github.com/kylebegeman/bedrock/internal/docker/dockertest"
	"github.com/kylebegeman/bedrock/internal/manifest"
	"github.com/kylebegeman/bedrock/internal/state"
)

// A backup of files the app can't write restores an app that answers and
// can't save, so the drill fails and says which files.
func TestADrillFailsWhenTheWorkloadCantWriteWhatWasRestored(t *testing.T) {
	stuck, open := t.TempDir(), t.TempDir()
	for _, dir := range []string{stuck, open} {
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	for dir, mode := range map[string]os.FileMode{stuck: 0o644, open: 0o666} {
		path := filepath.Join(dir, "app.sqlite")
		if err := os.WriteFile(path, []byte("x"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	mountpoints := map[string]string{"scratch-data": stuck, "scratch-uploads": open}
	mountpoint := func(v string) (string, error) {
		if dir, ok := mountpoints[v]; ok {
			return dir, nil
		}
		return "", errors.New("no such volume")
	}
	volumes := map[string]string{"data": "scratch-data", "uploads": "scratch-uploads", "gone": "scratch-gone"}
	w := manifest.Workload{Mounts: []manifest.Mount{{Volume: "data", Path: "/data"}, {Volume: "data", Path: "/also"}, {Volume: "uploads", Path: "/uploads"}}}
	someoneElse := docker.User{UID: os.Getuid() + 1, GID: os.Getgid() + 1}

	err := restoredWritable("web", w, someoneElse, volumes, mountpoint)
	if err == nil || !strings.Contains(err.Error(), "can't write 1 file or directory in volume data, such as app.sqlite") {
		t.Fatalf("the drill should fail on data: %v", err)
	}
	if strings.Count(err.Error(), "volume data") != 1 || strings.Contains(err.Error(), "volume uploads") {
		t.Fatalf("names each stuck volume once and only those: %v", err)
	}
	if err := restoredWritable("web", w, docker.User{UID: os.Getuid(), GID: os.Getgid()}, volumes, mountpoint); err != nil {
		t.Fatalf("the owner can write it all: %v", err)
	}
	if err := restoredWritable("web", w, docker.User{}, volumes, mountpoint); err != nil {
		t.Fatalf("root can write it all: %v", err)
	}
	lost := manifest.Workload{Mounts: []manifest.Mount{{Volume: "gone", Path: "/gone"}}}
	if err := restoredWritable("web", lost, someoneElse, volumes, mountpoint); err == nil {
		t.Fatal("a restored volume that can't be found is a failed drill, not a pass")
	}
}

func TestDrillUsesVerifiedRestoreSuccessAndCleansVerificationFailures(t *testing.T) {
	for _, fails := range []bool{false, true} {
		t.Run(map[bool]string{false: "ancestor progress counters", true: "verification failure"}[fails], func(t *testing.T) {
			r, _ := recoveryFixture(t)
			m := recoveryManifest()
			m.Data = &manifest.Data{Volumes: map[string]manifest.Volume{"uploads": {}}}
			snapshot := strings.Repeat("a", 64)
			d := &drillState{dr: Drill{Store: r.Store, Secrets: r.Secrets, StateDir: r.StateDir}, app: m.App, snapshot: snapshot, m: m,
				run: &backupRun{store: r.Store, app: m.App, kind: state.BackupRunDrill}, names: newDrillNames(r.StateDir, &m)}
			fake := dockertest.New(t)
			liveVolume := docker.VolumeName(m.App, "uploads")
			fake.AddVolume(liveVolume, nil)
			var restore dockertest.Container
			fake.OnCreate = func(c dockertest.Container) {
				fake.Update(c.Name, func(c *dockertest.Container) {
					c.ExitAfter = 1
					switch c.Cmd[0] {
					case "snapshots":
						c.Logs = `[{"id":"` + snapshot + `","short_id":"aaaaaaaa","time":"` + time.Now().UTC().Format(time.RFC3339) + `"}]`
					case "restore":
						restore = *c
						c.Logs = `{"message_type":"summary","files_restored":8,"total_files":6,"bytes_restored":12,"total_bytes":12}`
						if fails {
							c.ExitCode = 1
							c.Logs += "\n" + `{"message_type":"exit_error","message":"verification failed"}`
						}
					}
				})
			}
			err := d.restoreStep().Apply(context.Background(), io.Discard)
			if !slices.Contains(restore.Cmd, "--verify") {
				t.Error("drill restore must verify restored content")
			}
			if fails {
				if err == nil || !strings.Contains(err.Error(), "verification failed") {
					t.Fatalf("verification failure was accepted: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal("verified restore was rejected because of progress counters:", err)
				}
				if d.restored.FilesRestored != 8 || d.restored.TotalFiles != 6 {
					t.Fatalf("raw restore counters were changed: %+v", d.restored)
				}
				if err := d.cleanupStep().Apply(context.Background(), io.Discard); err != nil {
					t.Fatal(err)
				}
			}
			run, err := r.Store.LastBackupRun(context.Background(), m.App, state.BackupRunDrill)
			if err != nil || run == nil || !run.Finished() || run.OK == fails {
				t.Fatalf("drill receipt lost the verified outcome: %+v, %v", run, err)
			}
			if !fails && (run.Files != 8 || run.Bytes != 12) {
				t.Fatalf("drill receipt lost raw progress counters: %+v", run)
			}
			if fails && !strings.Contains(run.Error, "verification failed") {
				t.Fatalf("drill receipt lost verification error: %+v", run)
			}
			if !slices.Equal(fake.VolumeNames(), []string{liveVolume}) {
				t.Fatalf("scratch cleanup changed live data or leaked a volume: %v", fake.VolumeNames())
			}
			if _, err := os.Stat(d.names.dir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("scratch files outlived drill cleanup: %v", err)
			}
		})
	}
}
