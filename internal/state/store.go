// Package state is quark's one store: a SQLite file that journals every
// operation and its steps, so a crash can be resumed or undone and every
// change leaves a receipt.
package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // the pure-Go driver keeps the binary static
)

// Store is an open state database.
type Store struct {
	db   *sql.DB
	path string
}

// Open opens or creates the database at path, creating its directory with
// mode 0700. WAL mode lets the daemon write while the CLI reads.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("state directory: %w", err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, fmt.Errorf("open state: %w", err)
	}
	// One connection: SQLite serializes writers anyway, and this avoids
	// lock contention inside a single process.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Path is where the database lives.
func (s *Store) Path() string { return s.path }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS operations (
  id            TEXT PRIMARY KEY,
  kind          TEXT NOT NULL,
  target        TEXT NOT NULL,
  input         TEXT NOT NULL,
  plan          TEXT NOT NULL,
  plan_digest   TEXT NOT NULL,
  recovery      TEXT NOT NULL,
  status        TEXT NOT NULL,
  error         TEXT NOT NULL DEFAULT '',
  receipt       TEXT NOT NULL DEFAULT '',
  lease_owner   TEXT NOT NULL DEFAULT '',
  lease_until   INTEGER NOT NULL DEFAULT 0,
  interruptions INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  started_at    INTEGER NOT NULL DEFAULT 0,
  finished_at   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS operations_created ON operations (created_at DESC);
CREATE TABLE IF NOT EXISTS steps (
  operation_id TEXT NOT NULL REFERENCES operations(id) ON DELETE CASCADE,
  idx          INTEGER NOT NULL,
  name         TEXT NOT NULL,
  status       TEXT NOT NULL,
  attempts     INTEGER NOT NULL DEFAULT 0,
  error        TEXT NOT NULL DEFAULT '',
  output       TEXT NOT NULL DEFAULT '',
  started_at   INTEGER NOT NULL DEFAULT 0,
  finished_at  INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (operation_id, idx)
);`
	_, err := s.db.ExecContext(ctx, schema)
	if err != nil {
		return fmt.Errorf("migrate state: %w", err)
	}
	return nil
}

// OperationStatus is where an operation is in its life.
type OperationStatus string

const (
	// Planned means the plan is recorded and nothing has been applied.
	Planned OperationStatus = "planned"
	// Applying means steps are running, or were when the daemon died.
	Applying OperationStatus = "applying"
	// Succeeded means every step applied.
	Succeeded OperationStatus = "succeeded"
	// Failed means a step failed and nothing was undone.
	Failed OperationStatus = "failed"
	// Compensated means a step failed or the daemon died, and the completed
	// steps were undone.
	Compensated OperationStatus = "compensated"
)

// Final reports whether the status is terminal.
func (s OperationStatus) Final() bool {
	return s == Succeeded || s == Failed || s == Compensated
}

// StepStatus is where a step is in its life.
type StepStatus string

const (
	StepPending   StepStatus = "pending"
	StepRunning   StepStatus = "running"
	StepSucceeded StepStatus = "succeeded"
	StepFailed    StepStatus = "failed"
	// StepUndone means the step had succeeded and was then undone.
	StepUndone StepStatus = "undone"
)

// Operation is one journaled operation.
type Operation struct {
	ID            string
	Kind          string
	Target        string
	Input         json.RawMessage
	Plan          json.RawMessage
	PlanDigest    string
	Recovery      string
	Status        OperationStatus
	Error         string
	Receipt       json.RawMessage
	LeaseOwner    string
	LeaseUntil    time.Time
	Interruptions int
	CreatedAt     time.Time
	StartedAt     time.Time
	FinishedAt    time.Time
}

// Step is one journaled step of an operation.
type Step struct {
	OperationID string
	Index       int
	Name        string
	Status      StepStatus
	Attempts    int
	Error       string
	Output      string
	StartedAt   time.Time
	FinishedAt  time.Time
}

// NewOperation is what CreateOperation needs.
type NewOperation struct {
	ID         string
	Kind       string
	Target     string
	Input      json.RawMessage
	Plan       json.RawMessage
	PlanDigest string
	Recovery   string
	StepNames  []string
	CreatedAt  time.Time
}

// ErrNotFound is returned for an unknown operation.
var ErrNotFound = errors.New("no such operation")

// ErrLeased is returned when another owner holds a live lease.
var ErrLeased = errors.New("operation is leased by another owner")

// CreateOperation records a planned operation and its pending steps.
func (s *Store) CreateOperation(ctx context.Context, op NewOperation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO operations (id, kind, target, input, plan, plan_digest, recovery, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		op.ID, op.Kind, op.Target, string(op.Input), string(op.Plan), op.PlanDigest, op.Recovery, string(Planned), unix(op.CreatedAt))
	if err != nil {
		return fmt.Errorf("record operation: %w", err)
	}
	for i, name := range op.StepNames {
		if _, err := tx.ExecContext(ctx, `INSERT INTO steps (operation_id, idx, name, status) VALUES (?, ?, ?, ?)`, op.ID, i, name, string(StepPending)); err != nil {
			return fmt.Errorf("record step: %w", err)
		}
	}
	return tx.Commit()
}

// Claim takes the lease on an operation for owner until `until`, marks it
// applying, and counts an interruption when a previous owner had it. It
// fails with ErrLeased while another owner's lease is live.
func (s *Store) Claim(ctx context.Context, id, owner string, now, until time.Time) (*Operation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	op, err := scanOperation(tx.QueryRowContext(ctx, selectOperation+` WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	if op.Status.Final() {
		return nil, fmt.Errorf("operation %s is already %s", id, op.Status)
	}
	if op.LeaseOwner != "" && op.LeaseOwner != owner && op.LeaseUntil.After(now) {
		return nil, ErrLeased
	}
	interrupted := op.LeaseOwner != "" && op.LeaseOwner != owner
	interruptions := op.Interruptions
	if interrupted {
		interruptions++
	}
	startedAt := op.StartedAt
	if startedAt.IsZero() {
		startedAt = now
	}
	_, err = tx.ExecContext(ctx, `UPDATE operations SET status = ?, lease_owner = ?, lease_until = ?, interruptions = ?, started_at = ? WHERE id = ?`,
		string(Applying), owner, unix(until), interruptions, unix(startedAt), id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	op.Status = Applying
	op.LeaseOwner = owner
	op.LeaseUntil = until
	op.Interruptions = interruptions
	op.StartedAt = startedAt
	return op, nil
}

// Renew extends owner's lease. It fails when the lease was lost.
func (s *Store) Renew(ctx context.Context, id, owner string, until time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE operations SET lease_until = ? WHERE id = ? AND lease_owner = ? AND status = ?`, unix(until), id, owner, string(Applying))
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrLeased
	}
	return nil
}

// StepStarted marks a step running and counts the attempt.
func (s *Store) StepStarted(ctx context.Context, id string, index int, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE steps SET status = ?, attempts = attempts + 1, started_at = ?, error = '' WHERE operation_id = ? AND idx = ?`,
		string(StepRunning), unix(now), id, index)
	return err
}

// StepFinished records how a step ended and the tail of its output.
func (s *Store) StepFinished(ctx context.Context, id string, index int, status StepStatus, stepErr string, output string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE steps SET status = ?, error = ?, output = ?, finished_at = ? WHERE operation_id = ? AND idx = ?`,
		string(status), stepErr, output, unix(now), id, index)
	return err
}

// Finish records the operation's outcome and receipt and releases the lease.
func (s *Store) Finish(ctx context.Context, id string, status OperationStatus, opErr string, receipt json.RawMessage, now time.Time) error {
	if !status.Final() {
		return fmt.Errorf("%s is not a final status", status)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE operations SET status = ?, error = ?, receipt = ?, lease_owner = '', lease_until = 0, finished_at = ? WHERE id = ?`,
		string(status), opErr, string(receipt), unix(now), id)
	return err
}

// Get returns one operation and its steps.
func (s *Store) Get(ctx context.Context, id string) (*Operation, []Step, error) {
	op, err := scanOperation(s.db.QueryRowContext(ctx, selectOperation+` WHERE id = ?`, id))
	if err != nil {
		return nil, nil, err
	}
	steps, err := s.Steps(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return op, steps, nil
}

// Steps returns an operation's steps in order.
func (s *Store) Steps(ctx context.Context, id string) ([]Step, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT operation_id, idx, name, status, attempts, error, output, started_at, finished_at FROM steps WHERE operation_id = ? ORDER BY idx`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var steps []Step
	for rows.Next() {
		var st Step
		var started, finished int64
		if err := rows.Scan(&st.OperationID, &st.Index, &st.Name, &st.Status, &st.Attempts, &st.Error, &st.Output, &started, &finished); err != nil {
			return nil, err
		}
		st.StartedAt, st.FinishedAt = fromUnix(started), fromUnix(finished)
		steps = append(steps, st)
	}
	return steps, rows.Err()
}

// List returns the most recent operations, newest first.
func (s *Store) List(ctx context.Context, limit int) ([]Operation, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, selectOperation+` ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ops []Operation
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		ops = append(ops, *op)
	}
	return ops, rows.Err()
}

// Orphaned returns operations that were applying under a lease that has
// expired: the owner died or lost the lease. Oldest first.
func (s *Store) Orphaned(ctx context.Context, now time.Time) ([]Operation, error) {
	rows, err := s.db.QueryContext(ctx, selectOperation+` WHERE status = ? AND lease_until < ? ORDER BY created_at`, string(Applying), unix(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ops []Operation
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		ops = append(ops, *op)
	}
	return ops, rows.Err()
}

const selectOperation = `SELECT id, kind, target, input, plan, plan_digest, recovery, status, error, receipt, lease_owner, lease_until, interruptions, created_at, started_at, finished_at FROM operations`

type scanner interface {
	Scan(dest ...any) error
}

func scanOperation(row scanner) (*Operation, error) {
	var op Operation
	var input, plan, receipt string
	var leaseUntil, createdAt, startedAt, finishedAt int64
	err := row.Scan(&op.ID, &op.Kind, &op.Target, &input, &plan, &op.PlanDigest, &op.Recovery, &op.Status, &op.Error, &receipt,
		&op.LeaseOwner, &leaseUntil, &op.Interruptions, &createdAt, &startedAt, &finishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	op.Input, op.Plan = json.RawMessage(input), json.RawMessage(plan)
	if receipt != "" {
		op.Receipt = json.RawMessage(receipt)
	}
	op.LeaseUntil, op.CreatedAt, op.StartedAt, op.FinishedAt = fromUnix(leaseUntil), fromUnix(createdAt), fromUnix(startedAt), fromUnix(finishedAt)
	return &op, nil
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromUnix(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
