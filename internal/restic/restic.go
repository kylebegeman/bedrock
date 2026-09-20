// Package restic drives restic in a container: one repository per app in
// its own bucket, encrypted with the machine's backup password. bedrock
// mounts what to back up under /data and restores back into the same
// places, so a snapshot's paths mean the same thing on every machine.
package restic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kylebegeman/bedrock/internal/docker"
)

// Image is restic, pinned by digest.
const Image = "restic/restic:0.19.1@sha256:136600b6ff6843d61d355f7f71f460a166429f35de6fd11b568fece3c9a4d510"

// CacheVolume keeps restic's index cache between runs, for every repository.
const CacheVolume = "bedrock-restic-cache"

// DataRoot is where the backed-up tree sits inside the container.
const DataRoot = "/data"

// Tag marks snapshots bedrock made.
const Tag = "bedrock"

// Repo is one repository and how to open it.
type Repo struct {
	// Repository is restic's repository string, such as b2:bucket:/ .
	Repository string
	// Password encrypts the repository.
	Password string
	// Env carries the storage credentials, as restic reads them.
	Env []string
}

// Runner runs restic for one app.
type Runner struct {
	Engine *docker.Engine
	Repo   Repo
	// Owner is the app the containers are labeled with.
	Owner string
	// Log receives restic's lines, when set.
	Log func(line string)
}

// Summary is what restic reports after a backup.
type Summary struct {
	SnapshotID      string    `json:"snapshot_id"`
	FilesNew        int64     `json:"files_new"`
	FilesChanged    int64     `json:"files_changed"`
	FilesUnmodified int64     `json:"files_unmodified"`
	DataAdded       int64     `json:"data_added"`
	TotalFiles      int64     `json:"total_files_processed"`
	TotalBytes      int64     `json:"total_bytes_processed"`
	BackupStart     time.Time `json:"backup_start"`
	BackupEnd       time.Time `json:"backup_end"`
}

// Snapshot is one restic snapshot.
type Snapshot struct {
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
	Time     time.Time `json:"time"`
	Paths    []string  `json:"paths"`
	Hostname string    `json:"hostname"`
	Tags     []string  `json:"tags"`
	Summary  *Summary  `json:"summary,omitempty"`
}

// RestoreSummary is what restic reports after a restore.
type RestoreSummary struct {
	TotalFiles    int64 `json:"total_files"`
	FilesRestored int64 `json:"files_restored"`
	TotalBytes    int64 `json:"total_bytes"`
	BytesRestored int64 `json:"bytes_restored"`
}

// Keep is a retention policy.
type Keep struct {
	Daily, Weekly, Monthly int
}

// ErrNoRepository means the bucket holds no repository yet.
var ErrNoRepository = errors.New("no repository")

// run executes restic with the mounts and returns its output lines.
func (r Runner) run(ctx context.Context, mounts []string, args ...string) ([]string, error) {
	env := append([]string{"RESTIC_REPOSITORY=" + r.Repo.Repository, "RESTIC_PASSWORD=" + r.Repo.Password, "RESTIC_CACHE_DIR=/cache"}, r.Repo.Env...)
	spec := docker.Spec{
		Name:        fmt.Sprintf("bedrock-%s-restic-%d", r.Owner, time.Now().UnixMilli()),
		Image:       Image,
		Cmd:         args,
		Env:         env,
		Labels:      map[string]string{docker.LabelApp: r.Owner, docker.LabelWorkload: "backup"},
		Mounts:      append(append([]string{}, mounts...), CacheVolume+":/cache"),
		HostNetwork: true,
	}
	e := r.Engine
	if !e.HasImage(ctx, Image) {
		if err := e.Pull(ctx, Image, nopWriter{}); err != nil {
			return nil, err
		}
	}
	if err := e.Run(ctx, spec); err != nil {
		return nil, err
	}
	cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer e.Remove(cleanup, spec.Name, 5*time.Second) //nolint:errcheck // best effort
	code, err := e.WaitExit(ctx, spec.Name)
	var buf bytes.Buffer
	_ = e.Logs(cleanup, spec.Name, false, "all", &buf)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if err != nil {
		_ = e.Remove(cleanup, spec.Name, time.Second)
		return lines, err
	}
	if r.Log != nil {
		for _, l := range lines {
			if l != "" && !strings.Contains(l, `"message_type":"status"`) {
				r.Log(l)
			}
		}
	}
	if code != 0 {
		return lines, fmt.Errorf("restic %s: %s", args[0], explain(lines))
	}
	return lines, nil
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

// explain turns restic's last words into one line.
func explain(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		var msg struct {
			Type    string `json:"message_type"`
			Message string `json:"message"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &msg); err == nil && (msg.Type == "exit_error" || msg.Type == "error") {
			text := msg.Message
			if text == "" {
				text = msg.Error.Message
			}
			return strings.SplitN(strings.TrimPrefix(text, "Fatal: "), "\n", 2)[0]
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" && !strings.HasPrefix(t, "{") {
			return t
		}
	}
	return "failed without a message"
}

// Ensure makes sure the repository exists, reporting whether it was made.
// It asks restic to initialize the repository: on a store that lets it,
// that creates the bucket too, and on an existing repository it fails at
// once. (Asking restic to read a missing bucket instead would retry for
// a quarter of an hour, because it treats "no such bucket" as transient.)
func (r Runner) Ensure(ctx context.Context) (bool, error) {
	lines, err := r.run(ctx, nil, "init", "--json")
	if err == nil {
		return true, nil
	}
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "already initialized") || strings.Contains(joined, "already exists") {
		return false, nil
	}
	return false, err
}

// ListTimeout bounds the calls that only read the repository's index, so
// a bucket that doesn't answer is reported instead of waited on.
const ListTimeout = 5 * time.Minute

// Backup snapshots the mounted paths under a host name.
func (r Runner) Backup(ctx context.Context, host string, mounts []string, paths ...string) (*Summary, error) {
	args := append([]string{"backup", "--json", "--host", host, "--tag", Tag, "--one-file-system=false"}, paths...)
	lines, err := r.run(ctx, mounts, args...)
	if err != nil {
		return nil, err
	}
	for i := len(lines) - 1; i >= 0; i-- {
		var s struct {
			Type string `json:"message_type"`
			Summary
		}
		if json.Unmarshal([]byte(lines[i]), &s) == nil && s.Type == "summary" {
			return &s.Summary, nil
		}
	}
	return nil, errors.New("restic backup: no summary in the output")
}

// Snapshots lists a host's snapshots, newest first.
func (r Runner) Snapshots(ctx context.Context, host string) ([]Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, ListTimeout)
	defer cancel()
	lines, err := r.run(ctx, nil, "snapshots", "--json", "--host", host, "--tag", Tag)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("restic snapshots: no answer from the repository within %s (does the bucket exist, and do the credentials reach it?): %s", ListTimeout, explain(lines))
	}
	if err != nil {
		return nil, err
	}
	var out []Snapshot
	for _, l := range lines {
		if strings.HasPrefix(l, "[") {
			if err := json.Unmarshal([]byte(l), &out); err != nil {
				return nil, fmt.Errorf("restic snapshots: %w", err)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

// Latest returns the newest snapshot, or ErrNoRepository when there is
// none.
func (r Runner) Latest(ctx context.Context, host string) (*Snapshot, error) {
	snaps, err := r.Snapshots(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(snaps) == 0 {
		return nil, ErrNoRepository
	}
	return &snaps[0], nil
}

// Forget applies the retention policy and prunes what it freed. It
// returns how many snapshots were removed.
func (r Runner) Forget(ctx context.Context, host string, keep Keep) (int, error) {
	args := []string{"forget", "--json", "--host", host, "--tag", Tag, "--prune"}
	if keep.Daily > 0 {
		args = append(args, "--keep-daily", fmt.Sprint(keep.Daily))
	}
	if keep.Weekly > 0 {
		args = append(args, "--keep-weekly", fmt.Sprint(keep.Weekly))
	}
	if keep.Monthly > 0 {
		args = append(args, "--keep-monthly", fmt.Sprint(keep.Monthly))
	}
	lines, err := r.run(ctx, nil, args...)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, l := range lines {
		if !strings.HasPrefix(l, "[") {
			continue
		}
		var groups []struct {
			Remove []json.RawMessage `json:"remove"`
		}
		if json.Unmarshal([]byte(l), &groups) == nil {
			for _, g := range groups {
				removed += len(g.Remove)
			}
		}
	}
	return removed, nil
}

// Restore puts a snapshot's files back under /data, into whatever is
// mounted there. Includes limit it to paths inside the snapshot.
func (r Runner) Restore(ctx context.Context, snapshot, host string, mounts []string, includes ...string) (*RestoreSummary, error) {
	args := []string{"restore", snapshot, "--json", "--target", "/"}
	if snapshot == "latest" {
		args = append(args, "--host", host, "--tag", Tag)
	}
	for _, inc := range includes {
		args = append(args, "--include", inc)
	}
	lines, err := r.run(ctx, mounts, args...)
	if err != nil {
		return nil, err
	}
	for i := len(lines) - 1; i >= 0; i-- {
		var s struct {
			Type string `json:"message_type"`
			RestoreSummary
		}
		if json.Unmarshal([]byte(lines[i]), &s) == nil && s.Type == "summary" {
			return &s.RestoreSummary, nil
		}
	}
	return nil, errors.New("restic restore: no summary in the output")
}

// VolumeMount mounts a volume where a named volume lives in the tree.
func VolumeMount(volume, name string, readOnly bool) string {
	m := volume + ":" + DataRoot + "/volumes/" + name
	if readOnly {
		m += ":ro"
	}
	return m
}

// VolumePath is where a named volume lives in the tree.
func VolumePath(name string) string { return DataRoot + "/volumes/" + name }

// DirMount mounts a host directory as the tree's root.
func DirMount(dir string, readOnly bool) string {
	m := dir + ":" + DataRoot
	if readOnly {
		m += ":ro"
	}
	return m
}
