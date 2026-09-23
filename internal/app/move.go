package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kylebegeman/bedrock/internal/integration"
	"github.com/kylebegeman/bedrock/internal/manifest"
	"github.com/kylebegeman/bedrock/internal/secrets"
	"github.com/kylebegeman/bedrock/internal/state"
)

// Handoff is everything a machine needs to take an app over from another
// machine: which app, which snapshot of its data, and where that snapshot
// is.
//
// It carries no secrets. Every field is a name, an identifier or a
// hostname, so a handoff can be printed, read over a shoulder and kept in a
// log. What it deliberately does not carry is the way into the bucket: the
// target reads the snapshot with its own storage integration, which is why
// a move requires both machines to be pointed at the same storage rather
// than one machine handing the other a key.
type Handoff struct {
	// App is the app being moved.
	App string `json:"app"`
	// From is the machine it is moving off, for the record.
	From string `json:"from"`
	// Bucket is where the data was left. The target must reach the same
	// one, under the same repository password, or it will find nothing.
	Bucket string `json:"bucket"`
	// Snapshot is the restic snapshot the target should restore. Naming it
	// rather than taking the latest means the target restores the data the
	// source actually finished writing, not whatever arrived afterwards.
	Snapshot string `json:"snapshot,omitempty"`
	// TakenAt is that snapshot's recovery point.
	TakenAt time.Time `json:"taken_at,omitempty"`
	// HasData says whether the app keeps anything. An app with none moves
	// without a snapshot at all.
	HasData bool `json:"has_data"`
	// Hosts are the names that have to end up pointing at the target.
	Hosts []string `json:"hosts,omitempty"`
	// Repo is where the source lives, when the app declares one; the target
	// needs the source to build from.
	Repo string `json:"repo,omitempty"`
	// Revision is what was running on the source when this was written.
	Revision string `json:"revision,omitempty"`
	// At is when the handoff was written.
	At time.Time `json:"at"`
}

// ErrNoSnapshot says an app that keeps data has no backup to move it by.
var ErrNoSnapshot = errors.New("no good backup to move")

// NewHandoff describes what it would take to move an app off this machine.
// It reads only; taking the final backup is the caller's job, and should
// happen first so the snapshot named here is the last one.
func NewHandoff(ctx context.Context, store *state.Store, sec *secrets.Store, app, from string) (*Handoff, error) {
	rev, err := store.RevisionWithStatus(ctx, app, state.RevisionActive)
	if err != nil {
		return nil, err
	}
	if rev == nil {
		return nil, fmt.Errorf("%s isn't running here, so there is nothing to move", app)
	}
	m, err := manifest.Parse(rev.Manifest)
	if err != nil {
		return nil, fmt.Errorf("%s: the running revision's manifest doesn't parse: %w", app, err)
	}
	h := &Handoff{
		App: app, From: from, HasData: m.HasData(),
		Hosts: m.Hosts(), Repo: m.Repo, Revision: rev.ID, At: time.Now().UTC(),
	}
	st, err := integration.LoadStorage(sec)
	if err == nil && st.BucketPrefix != "" {
		h.Bucket = st.Bucket(app)
	} else if h.HasData {
		return nil, fmt.Errorf("%s keeps data and this machine has no storage integration, so there is no snapshot to move it by", app)
	}
	if !h.HasData {
		return h, nil
	}
	run, err := store.LastGoodBackupRun(ctx, app, state.BackupRunBackup)
	if err != nil {
		return nil, err
	}
	if run == nil || run.Snapshot == "" {
		return nil, fmt.Errorf("%w: back %s up first, so the target restores what it was actually running", ErrNoSnapshot, app)
	}
	h.Snapshot, h.TakenAt = run.Snapshot, run.SnapshotAt
	return h, nil
}

// Check reports whether this machine can act on the handoff, naming what is
// wrong in terms of what to change rather than what was compared.
//
// The bucket check is the one that matters. Two machines set up separately
// get different bucket prefixes and, unless a password was typed in twice,
// different repository passwords, so a target pointed at its own storage
// looks in the wrong place and finds nothing. That reads as an empty app
// rather than as a failure, which is the worst way for a move to go wrong.
func (h *Handoff) Check(app string, st *integration.Storage) error {
	switch {
	case h.App == "":
		return errors.New("the handoff names no app")
	case app != "" && h.App != app:
		return fmt.Errorf("this handoff is for %s, not %s", h.App, app)
	case !h.HasData:
		return nil
	case h.Bucket == "":
		return fmt.Errorf("the handoff for %s says it keeps data but names no bucket", h.App)
	case h.Snapshot == "":
		return fmt.Errorf("the handoff for %s names no snapshot", h.App)
	case st == nil || st.BucketPrefix == "":
		return errors.New("this machine has no storage integration; set one that reads the same bucket: bedrock integration set storage")
	case st.Bucket(h.App) != h.Bucket:
		return fmt.Errorf("the handoff points at %s and this machine reads %s; both machines must use the same storage integration, the same bucket prefix and the same backup password",
			h.Bucket, st.Bucket(h.App))
	}
	return nil
}

// WriteHandoff prints a handoff as the JSON a target reads back.
func WriteHandoff(w io.Writer, h *Handoff) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(h)
}

// ReadHandoff parses a handoff.
func ReadHandoff(r io.Reader) (*Handoff, error) {
	var h Handoff
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil {
		return nil, fmt.Errorf("this isn't a handoff: %w", err)
	}
	if strings.TrimSpace(h.App) == "" {
		return nil, errors.New("this isn't a handoff: it names no app")
	}
	return &h, nil
}

// Describe is the handoff in the words the person moving the app needs.
func (h *Handoff) Describe() []string {
	lines := []string{fmt.Sprintf("%s, as it stands on %s", h.App, orUnknownFrom(h.From))}
	if h.HasData {
		lines = append(lines, fmt.Sprintf("data in %s, snapshot %s taken %s", h.Bucket, h.Snapshot, h.TakenAt.Format(time.RFC3339)))
	} else {
		lines = append(lines, "keeps no data, so nothing has to be restored")
	}
	if len(h.Hosts) > 0 {
		lines = append(lines, "hosts to repoint: "+strings.Join(h.Hosts, ", "))
	}
	if h.Repo != "" {
		lines = append(lines, "source: "+h.Repo)
	}
	return lines
}

func orUnknownFrom(s string) string {
	if s == "" {
		return "the source machine"
	}
	return s
}
