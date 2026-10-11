package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ReleaseBoundary records that a candidate may have changed the app's data.
// Failure does not mean that SQL or volume writes were reversed.
type ReleaseBoundary struct {
	App              string    `json:"app"`
	Revision         string    `json:"revision"`
	PreviousRevision string    `json:"previous_revision,omitempty"`
	Compatibility    string    `json:"compatibility"`
	Snapshot         string    `json:"snapshot,omitempty"`
	Status           string    `json:"status"`
	StartedAt        time.Time `json:"started_at"`
	FinishedAt       time.Time `json:"finished_at,omitempty"`
	Error            string    `json:"error,omitempty"`
}

const (
	ReleasePrepared  = "prepared"
	ReleaseRestoring = "restoring"
	ReleaseStarted   = "started"
	ReleaseCompleted = "completed"
	ReleaseFailed    = "failed"
	ReleaseRestored  = "restored"
)

func (s *Store) migrateReleaseBoundaries(ctx context.Context, q migrationDB) error {
	_, err := q.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS release_boundaries (
 app TEXT NOT NULL,
 revision TEXT NOT NULL,
 previous_revision TEXT NOT NULL DEFAULT '',
 compatibility TEXT NOT NULL,
 snapshot TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL,
 started_at INTEGER NOT NULL,
 finished_at INTEGER NOT NULL DEFAULT 0,
 error TEXT NOT NULL DEFAULT '',
 PRIMARY KEY (app, revision)
 ); CREATE INDEX IF NOT EXISTS release_boundaries_app ON release_boundaries(app, started_at DESC)`)
	return err
}

// BeginReleaseBoundary writes the boundary before any release job starts.
// A restart adopts the same boundary; it must not erase an uncertain outcome.
func (s *Store) BeginReleaseBoundary(ctx context.Context, b ReleaseBoundary) error {
	if b.App == "" || b.Revision == "" || b.Compatibility == "" || b.StartedAt.IsZero() {
		return errors.New("release boundary needs app, revision, compatibility and start time")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO release_boundaries (app,revision,previous_revision,compatibility,snapshot,status,started_at) VALUES (?,?,?,?,?,?,?) ON CONFLICT(app,revision) DO NOTHING`, b.App, b.Revision, b.PreviousRevision, b.Compatibility, b.Snapshot, ReleaseStarted, unix(b.StartedAt))
	return err
}

// FinishReleaseBoundary records a completed or failed mutation. Data remains
// changed or uncertain in both cases until an explicit checkpoint restore.
func (s *Store) FinishReleaseBoundary(ctx context.Context, app, revision, status, message string, at time.Time) error {
	if status != ReleaseCompleted && status != ReleaseFailed && status != ReleaseRestored {
		return fmt.Errorf("invalid release boundary outcome %q", status)
	}
	result, err := s.db.ExecContext(ctx, `UPDATE release_boundaries SET status=?,finished_at=?,error=? WHERE app=? AND revision=?`, status, unix(at), message, app, revision)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrNotFound
	}
	return nil
}

// ReleaseBoundaries includes failed and interrupted attempts, because a release
// can alter data before any candidate revision becomes active.
func (s *Store) ReleaseBoundaries(ctx context.Context, app string) ([]ReleaseBoundary, error) {
	query := `SELECT app,revision,previous_revision,compatibility,snapshot,status,started_at,finished_at,error FROM release_boundaries`
	var args []any
	if app != "" {
		query += ` WHERE app=?`
		args = append(args, app)
	}
	query += ` ORDER BY started_at DESC,revision DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []ReleaseBoundary
	for rows.Next() {
		var b ReleaseBoundary
		var started, finished int64
		if err := rows.Scan(&b.App, &b.Revision, &b.PreviousRevision, &b.Compatibility, &b.Snapshot, &b.Status, &started, &finished, &b.Error); err != nil {
			return nil, err
		}
		b.StartedAt, b.FinishedAt = fromUnix(started), fromUnix(finished)
		result = append(result, b)
	}
	return result, rows.Err()
}

// ReleaseBoundary returns one recorded data transition.
func (s *Store) ReleaseBoundary(ctx context.Context, app, revision string) (*ReleaseBoundary, error) {
	var b ReleaseBoundary
	var started, finished int64
	err := s.db.QueryRowContext(ctx, `SELECT app,revision,previous_revision,compatibility,snapshot,status,started_at,finished_at,error FROM release_boundaries WHERE app=? AND revision=?`, app, revision).Scan(&b.App, &b.Revision, &b.PreviousRevision, &b.Compatibility, &b.Snapshot, &b.Status, &started, &finished, &b.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b.StartedAt, b.FinishedAt = fromUnix(started), fromUnix(finished)
	return &b, nil
}

// PrepareReleaseBoundary pins the checkpoint before release jobs are admitted.
// Replanning preserves the original checkpoint even if another backup exists.
func (s *Store) PrepareReleaseBoundary(ctx context.Context, b ReleaseBoundary) error {
	if b.App == "" || b.Revision == "" || b.Compatibility == "" || b.StartedAt.IsZero() {
		return errors.New("release checkpoint needs app, revision, compatibility and time")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO release_boundaries (app,revision,previous_revision,compatibility,snapshot,status,started_at) VALUES (?,?,?,?,?,?,?) ON CONFLICT(app,revision) DO NOTHING`, b.App, b.Revision, b.PreviousRevision, b.Compatibility, b.Snapshot, ReleasePrepared, unix(b.StartedAt))
	return err
}

// StartPreparedReleaseBoundary transitions exactly once. A running or failed
// attempt must never be replayed, because its data writes are uncertain.
func (s *Store) StartPreparedReleaseBoundary(ctx context.Context, app, revision string, at time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE release_boundaries SET status=?,started_at=? WHERE app=? AND revision=? AND status=?`, ReleaseStarted, unix(at), app, revision, ReleasePrepared)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// BeginReleaseRestore persists the writer fence before live data is replaced.
func (s *Store) BeginReleaseRestore(ctx context.Context, app, revision, snapshot string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE release_boundaries SET status=?,error=? WHERE app=? AND revision=? AND snapshot=? AND status<>?`, ReleaseRestoring, "checkpoint restoration is in progress", app, revision, snapshot, ReleaseRestored)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE revisions SET status=? WHERE app=? AND status=?`, RevisionRetired, app, RevisionActive); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE apps SET active_revision='' WHERE name=?`, app); err != nil {
		return err
	}
	return tx.Commit()

}

// CompleteReleaseRestore atomically retires active code and marks every data
// transition after the chosen checkpoint restored. Revisions and secrets stay.
func (s *Store) CompleteReleaseRestore(ctx context.Context, b ReleaseBoundary, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM release_boundaries WHERE app=? AND revision=? AND snapshot=?`, b.App, b.Revision, b.Snapshot).Scan(&status); err != nil {
		return err
	}
	if status != ReleaseRestoring {
		return errors.New("release checkpoint restoration is not fenced")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE revisions SET status=? WHERE app=? AND status=?`, RevisionRetired, b.App, RevisionActive); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE apps SET active_revision='',updated_at=? WHERE name=?`, unix(at), b.App); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE release_boundaries SET status=?,finished_at=?,error='' WHERE app=? AND started_at>=?`, ReleaseRestored, unix(at), b.App, unix(b.StartedAt)); err != nil {
		return err
	}
	return tx.Commit()
}
