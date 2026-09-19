// Package api is how the CLI talks to a daemon: JSON over a Unix socket,
// with operations streamed as one event per line. Local is the same
// contract without a daemon, for a machine that has none (the Mac).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/kylebegeman/quark/internal/kernel"
	"github.com/kylebegeman/quark/internal/state"
	"github.com/kylebegeman/quark/internal/version"
)

// Summary is one operation as history lists it.
type Summary struct {
	ID            string                `json:"id"`
	Kind          string                `json:"kind"`
	Target        string                `json:"target"`
	Status        state.OperationStatus `json:"status"`
	CreatedAt     time.Time             `json:"created_at"`
	FinishedAt    time.Time             `json:"finished_at,omitempty"`
	Interruptions int                   `json:"interruptions"`
	Error         string                `json:"error,omitempty"`
}

func summarize(op state.Operation) Summary {
	return Summary{ID: op.ID, Kind: op.Kind, Target: op.Target, Status: op.Status, CreatedAt: op.CreatedAt, FinishedAt: op.FinishedAt, Interruptions: op.Interruptions, Error: op.Error}
}

// Runner is what the CLI needs, whether a daemon or the local kernel serves it.
type Runner interface {
	Version(ctx context.Context) (version.Info, error)
	Plan(ctx context.Context, kind string, input json.RawMessage) (*kernel.PlanView, error)
	Run(ctx context.Context, kind string, input json.RawMessage, emit func(kernel.Event)) (*kernel.Receipt, error)
	List(ctx context.Context, limit int) ([]Summary, error)
	Receipt(ctx context.Context, id string) (*kernel.Receipt, error)
	Close() error
}

// RunRequest is the body of POST /v1/operations.
type RunRequest struct {
	Kind     string          `json:"kind"`
	Input    json.RawMessage `json:"input"`
	PlanOnly bool            `json:"plan_only,omitempty"`
}

// Server serves the API for one engine. Operations run one at a time: the
// daemon is the single authority on its machine.
type Server struct {
	Engine *kernel.Engine
	Store  *state.Store
	mu     sync.Mutex
}

// Recover finishes operations left behind by a daemon that died, one at a
// time with everything else the server runs. The daemon calls it at start
// and then keeps sweeping, because a dead daemon's lease can outlive its
// restart.
func (s *Server) Recover(ctx context.Context, emit func(kernel.Event)) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Engine.Recover(ctx, emit)
}

// RunLocked runs an operation the daemon starts on its own clock, such as
// a scheduled backup, one at a time with everything else the server runs.
func (s *Server) RunLocked(ctx context.Context, kind string, input json.RawMessage, emit func(kernel.Event)) (*kernel.Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Engine.Run(ctx, kind, input, emit)
}

// Handler returns the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, version.Current())
	})
	mux.HandleFunc("GET /v1/operations", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		ops, err := s.Store.List(r.Context(), limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		rows := make([]Summary, 0, len(ops))
		for _, op := range ops {
			rows = append(rows, summarize(op))
		}
		writeJSON(w, http.StatusOK, rows)
	})
	mux.HandleFunc("GET /v1/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		receipt, err := s.Engine.ReceiptOf(r.Context(), r.PathValue("id"))
		if errors.Is(err, state.ErrNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, receipt)
	})
	mux.HandleFunc("POST /v1/operations", s.runOperation)
	return mux
}

func (s *Server) runOperation(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bad request: %w", err))
		return
	}
	if req.PlanOnly {
		view, err := s.Engine.PlanOnly(r.Context(), req.Kind, req.Input)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(w).Encode(kernel.Event{Type: kernel.EventPlanned, At: time.Now().UTC(), Plan: view})
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	emit := func(ev kernel.Event) {
		_ = enc.Encode(ev)
		if flusher != nil {
			flusher.Flush()
		}
	}
	// The client going away must not stop the operation: it is journaled
	// and finishes on the daemon's own clock.
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.Engine.Run(context.WithoutCancel(r.Context()), req.Kind, req.Input, emit); err != nil {
		emit(kernel.Event{Type: kernel.EventError, At: time.Now().UTC(), Message: err.Error()})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// Listen opens the daemon's Unix socket, replacing a stale one, readable
// and writable by its owner and group only.
func Listen(socket string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(socket), 0o750); err != nil {
		return nil, err
	}
	if _, err := os.Stat(socket); err == nil {
		// A live daemon would answer; a stale socket file is just a file.
		if conn, err := net.DialTimeout("unix", socket, 500*time.Millisecond); err == nil {
			conn.Close()
			return nil, fmt.Errorf("another daemon is listening on %s", socket)
		}
		_ = os.Remove(socket)
	}
	l, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socket, 0o660); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// Local serves the Runner contract from an in-process kernel.
type Local struct {
	Engine *kernel.Engine
	Store  *state.Store
}

// Version implements Runner.
func (l *Local) Version(context.Context) (version.Info, error) { return version.Current(), nil }

// Plan implements Runner.
func (l *Local) Plan(ctx context.Context, kind string, input json.RawMessage) (*kernel.PlanView, error) {
	return l.Engine.PlanOnly(ctx, kind, input)
}

// Run implements Runner.
func (l *Local) Run(ctx context.Context, kind string, input json.RawMessage, emit func(kernel.Event)) (*kernel.Receipt, error) {
	return l.Engine.Run(ctx, kind, input, emit)
}

// List implements Runner.
func (l *Local) List(ctx context.Context, limit int) ([]Summary, error) {
	ops, err := l.Store.List(ctx, limit)
	if err != nil {
		return nil, err
	}
	rows := make([]Summary, 0, len(ops))
	for _, op := range ops {
		rows = append(rows, summarize(op))
	}
	return rows, nil
}

// Receipt implements Runner.
func (l *Local) Receipt(ctx context.Context, id string) (*kernel.Receipt, error) {
	return l.Engine.ReceiptOf(ctx, id)
}

// Close implements Runner.
func (l *Local) Close() error { return l.Store.Close() }
