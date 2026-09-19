package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// RevisionStatus is where a revision is in its life.
type RevisionStatus string

const (
	// RevisionActive is the one the edge routes to.
	RevisionActive RevisionStatus = "active"
	// RevisionPrevious is kept ready for rollback.
	RevisionPrevious RevisionStatus = "previous"
	// RevisionRetired is history; its containers are gone.
	RevisionRetired RevisionStatus = "retired"
	// RevisionFailed never went live.
	RevisionFailed RevisionStatus = "failed"
)

// Revision is one deployed version of an app.
type Revision struct {
	App        string            `json:"app"`
	ID         string            `json:"id"`
	Status     RevisionStatus    `json:"status"`
	Manifest   json.RawMessage   `json:"manifest"`
	Images     map[string]string `json:"images"`
	Containers map[string]string `json:"containers"`
	Source     string            `json:"source,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
}

// App is one app quark runs.
type App struct {
	Name           string    `json:"name"`
	ActiveRevision string    `json:"active_revision,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func (s *Store) migrateApps(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS apps (
  name            TEXT PRIMARY KEY,
  active_revision TEXT NOT NULL DEFAULT '',
  updated_at      INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS revisions (
  app        TEXT NOT NULL,
  id         TEXT NOT NULL,
  status     TEXT NOT NULL,
  manifest   TEXT NOT NULL,
  images     TEXT NOT NULL,
  containers TEXT NOT NULL,
  source     TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  PRIMARY KEY (app, id)
);`
	_, err := s.db.ExecContext(ctx, schema)
	return err
}

// SaveRevision records a revision, replacing one with the same app and id.
func (s *Store) SaveRevision(ctx context.Context, r Revision) error {
	images, err := json.Marshal(r.Images)
	if err != nil {
		return err
	}
	containers, err := json.Marshal(r.Containers)
	if err != nil {
		return err
	}
	if r.Manifest == nil {
		r.Manifest = json.RawMessage(`{}`)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO revisions (app, id, status, manifest, images, containers, source, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(app, id) DO UPDATE SET status = excluded.status, manifest = excluded.manifest, images = excluded.images, containers = excluded.containers, source = excluded.source`,
		r.App, r.ID, string(r.Status), string(r.Manifest), string(images), string(containers), r.Source, unix(r.CreatedAt))
	return err
}

// SetRevisionStatus changes one revision's status.
func (s *Store) SetRevisionStatus(ctx context.Context, app, id string, status RevisionStatus) error {
	_, err := s.db.ExecContext(ctx, `UPDATE revisions SET status = ? WHERE app = ? AND id = ?`, string(status), app, id)
	return err
}

// Activate makes a revision the app's active one: the old active becomes
// previous, the old previous becomes retired.
func (s *Store) Activate(ctx context.Context, app, id string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE revisions SET status = ? WHERE app = ? AND status = ? AND id <> ?`, string(RevisionRetired), app, string(RevisionPrevious), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE revisions SET status = ? WHERE app = ? AND status = ? AND id <> ?`, string(RevisionPrevious), app, string(RevisionActive), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE revisions SET status = ? WHERE app = ? AND id = ?`, string(RevisionActive), app, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO apps (name, active_revision, updated_at) VALUES (?, ?, ?) ON CONFLICT(name) DO UPDATE SET active_revision = excluded.active_revision, updated_at = excluded.updated_at`, app, id, unix(now)); err != nil {
		return err
	}
	return tx.Commit()
}

// GetRevision returns one revision.
func (s *Store) GetRevision(ctx context.Context, app, id string) (*Revision, error) {
	row := s.db.QueryRowContext(ctx, `SELECT app, id, status, manifest, images, containers, source, created_at FROM revisions WHERE app = ? AND id = ?`, app, id)
	return scanRevision(row)
}

// Revisions lists an app's revisions, newest first.
func (s *Store) Revisions(ctx context.Context, app string) ([]Revision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT app, id, status, manifest, images, containers, source, created_at FROM revisions WHERE app = ? ORDER BY created_at DESC, id DESC`, app)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Revision
	for rows.Next() {
		r, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// RevisionWithStatus returns an app's revision in a status, or nil.
func (s *Store) RevisionWithStatus(ctx context.Context, app string, status RevisionStatus) (*Revision, error) {
	row := s.db.QueryRowContext(ctx, `SELECT app, id, status, manifest, images, containers, source, created_at FROM revisions WHERE app = ? AND status = ? ORDER BY created_at DESC LIMIT 1`, app, string(status))
	r, err := scanRevision(row)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return r, err
}

// Apps lists every app, with its active revision.
func (s *Store) Apps(ctx context.Context) ([]App, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, active_revision, updated_at FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		var a App
		var updated int64
		if err := rows.Scan(&a.Name, &a.ActiveRevision, &updated); err != nil {
			return nil, err
		}
		a.UpdatedAt = fromUnix(updated)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ActiveRevisions returns every app's active revision.
func (s *Store) ActiveRevisions(ctx context.Context) ([]Revision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT app, id, status, manifest, images, containers, source, created_at FROM revisions WHERE status = ? ORDER BY app`, string(RevisionActive))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Revision
	for rows.Next() {
		r, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// RemoveApp forgets an app and its revisions.
func (s *Store) RemoveApp(ctx context.Context, app string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM revisions WHERE app = ?`, app); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM apps WHERE name = ?`, app); err != nil {
		return err
	}
	return tx.Commit()
}

func scanRevision(row scanner) (*Revision, error) {
	var r Revision
	var manifest, images, containers string
	var created int64
	err := row.Scan(&r.App, &r.ID, &r.Status, &manifest, &images, &containers, &r.Source, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.Manifest = json.RawMessage(manifest)
	if err := json.Unmarshal([]byte(images), &r.Images); err != nil {
		return nil, fmt.Errorf("revision %s/%s images: %w", r.App, r.ID, err)
	}
	if err := json.Unmarshal([]byte(containers), &r.Containers); err != nil {
		return nil, fmt.Errorf("revision %s/%s containers: %w", r.App, r.ID, err)
	}
	r.CreatedAt = fromUnix(created)
	return &r, nil
}
