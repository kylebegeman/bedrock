package state

import (
	"context"
	"time"
)

// GitHook is one app's webhook: pushes to Branch of Repo deploy the app.
type GitHook struct {
	App        string    `json:"app"`
	Repo       string    `json:"repo"`
	Branch     string    `json:"branch"`
	CreatedAt  time.Time `json:"created_at"`
	LastCommit string    `json:"last_commit,omitempty"`
	LastResult string    `json:"last_result,omitempty"`
	LastAt     time.Time `json:"last_at,omitempty"`
}

func (s *Store) migrateGitHooks(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS git_hooks (
  app         TEXT PRIMARY KEY,
  repo        TEXT NOT NULL,
  branch      TEXT NOT NULL,
  created_at  INTEGER NOT NULL,
  last_commit TEXT NOT NULL DEFAULT '',
  last_result TEXT NOT NULL DEFAULT '',
  last_at     INTEGER NOT NULL DEFAULT 0
);`)
	return err
}

// SaveGitHook records an app's webhook, replacing an earlier one.
func (s *Store) SaveGitHook(ctx context.Context, h GitHook) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO git_hooks (app, repo, branch, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(app) DO UPDATE SET repo = excluded.repo, branch = excluded.branch, created_at = excluded.created_at, last_commit = '', last_result = '', last_at = 0`,
		h.App, h.Repo, h.Branch, unix(h.CreatedAt))
	return err
}

// RecordGitHook notes what the last delivery did.
func (s *Store) RecordGitHook(ctx context.Context, app, commit, result string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE git_hooks SET last_commit = ?, last_result = ?, last_at = ? WHERE app = ?`, commit, result, unix(at), app)
	return err
}

// RemoveGitHook forgets an app's webhook. ErrNotFound when it had none.
func (s *Store) RemoveGitHook(ctx context.Context, app string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM git_hooks WHERE app = ?`, app)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GitHooks lists every webhook.
func (s *Store) GitHooks(ctx context.Context) ([]GitHook, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT app, repo, branch, created_at, last_commit, last_result, last_at FROM git_hooks ORDER BY app`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GitHook
	for rows.Next() {
		var h GitHook
		var created, last int64
		if err := rows.Scan(&h.App, &h.Repo, &h.Branch, &created, &h.LastCommit, &h.LastResult, &last); err != nil {
			return nil, err
		}
		h.CreatedAt, h.LastAt = fromUnix(created), fromUnix(last)
		out = append(out, h)
	}
	return out, rows.Err()
}

// GitHookFor returns an app's webhook, or nil.
func (s *Store) GitHookFor(ctx context.Context, app string) (*GitHook, error) {
	hooks, err := s.GitHooks(ctx)
	if err != nil {
		return nil, err
	}
	for _, h := range hooks {
		if h.App == app {
			return &h, nil
		}
	}
	return nil, nil
}
