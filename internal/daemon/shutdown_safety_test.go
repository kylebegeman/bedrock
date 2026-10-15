package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kylebegeman/helios/internal/api"
	"github.com/kylebegeman/helios/internal/gitdeploy"
	"github.com/kylebegeman/helios/internal/kernel"
	"github.com/kylebegeman/helios/internal/state"
)

type streamingDefinition struct {
	started chan struct{}
	writes  atomic.Int64
}

func (*streamingDefinition) Kind() string { return "test.shutdown-stream" }
func (d *streamingDefinition) Plan(context.Context, json.RawMessage) (*kernel.Plan, error) {
	return &kernel.Plan{Target: "shutdown", Steps: []kernel.Step{{Name: "stream", Apply: func(ctx context.Context, out io.Writer) error {
		close(d.started)
		line := strings.Repeat("x", 60<<10) + "\n"
		for ctx.Err() == nil {
			if _, err := io.WriteString(out, line); err != nil {
				return err
			}
			d.writes.Add(1)
		}
		return ctx.Err()
	}}}}, nil
}

func TestShutdownClosesAClientThatStopsReadingEvents(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	t.Setenv("WATCHDOG_USEC", "")
	t.Setenv("WATCHDOG_PID", "")
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	socketDir, err := os.MkdirTemp("/tmp", "bedrock-shutdown-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	def := &streamingDefinition{started: make(chan struct{})}
	reg := kernel.Registry{}
	reg.Add(def)
	ready := make(chan struct{})
	d := &daemon{cfg: Config{Socket: filepath.Join(socketDir, "api.sock")}, store: store, cancel: cancel,
		api: &api.Server{Store: store, Engine: kernel.New(store, reg, "test"), Lifetime: ctx},
		logf: func(format string, _ ...any) {
			if strings.HasPrefix(format, "listening on ") {
				close(ready)
			}
		},
	}
	served := make(chan struct{})
	var serveErr error
	go func() { serveErr = d.serveAPI(ctx, &gitdeploy.Receiver{}); close(served) }()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("API did not listen")
	}
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: d.cfg.Socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		conn.Close()
		cancel()
		select {
		case <-served:
		case <-time.After(7 * time.Second):
			t.Error("API did not stop during cleanup")
		}
		drainCtx, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		if err := d.api.Drain(drainCtx); err != nil {
			t.Errorf("API did not drain during cleanup: %v", err)
		}
	}()
	if err := conn.SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"kind":%q,"input":null}`, def.Kind())
	if _, err := fmt.Fprintf(conn, "POST /v1/operations HTTP/1.1\r\nHost: local\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body); err != nil {
		t.Fatal(err)
	}
	select {
	case <-def.started:
	case <-time.After(time.Second):
		t.Fatal("streaming operation did not start")
	}
	// Wait until the server fills the socket, without reading any response.
	last := def.writes.Load()
	stableSince := time.Now()
	deadline := time.Now().Add(2 * time.Second)
	for {
		current := def.writes.Load()
		if current != last {
			last, stableSince = current, time.Now()
		}
		if time.Since(stableSince) >= 50*time.Millisecond {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stream did not fill the socket")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-served:
		if serveErr != nil {
			t.Fatal(serveErr)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("API did not stop within its shutdown grace")
	}
	drainCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := d.api.Drain(drainCtx); err != nil {
		t.Fatalf("shutdown left an event write blocking operation drain: %v", err)
	}
}
