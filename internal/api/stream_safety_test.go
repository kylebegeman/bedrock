package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/bedrock/internal/kernel"
	"github.com/kylebegeman/bedrock/internal/state"
)

type noisyStreamDefinition struct{ done chan struct{} }

func (*noisyStreamDefinition) Kind() string { return "test.finite-stream" }
func (d *noisyStreamDefinition) Plan(context.Context, json.RawMessage) (*kernel.Plan, error) {
	return &kernel.Plan{Target: "stream", Steps: []kernel.Step{{Name: "output", Apply: func(ctx context.Context, out io.Writer) error {
		line := strings.Repeat("x", 60<<10) + "\n"
		for range 256 {
			if _, err := io.WriteString(out, line); err != nil {
				return err
			}
		}
		close(d.done)
		return nil
	}}}}, nil
}

func TestNonreadingEventClientCannotBlockAnOperation(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	dir, err := os.MkdirTemp("/tmp", "bedrock-stream-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	listener, err := net.Listen("unix", filepath.Join(dir, "api.sock"))
	if err != nil {
		t.Fatal(err)
	}
	def := &noisyStreamDefinition{done: make(chan struct{})}
	reg := kernel.Registry{}
	reg.Add(def)
	api := &Server{Store: store, Engine: kernel.New(store, reg, "test")}
	server := &http.Server{Handler: api.Handler()}
	defer server.Close()
	go server.Serve(listener)
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: listener.Addr().String(), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetReadBuffer(1024); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"kind":%q,"input":null}`, def.Kind())
	if _, err := fmt.Fprintf(conn, "POST /v1/operations HTTP/1.1\r\nHost: local\r\nContent-Length: %d\r\n\r\n%s", len(body), body); err != nil {
		t.Fatal(err)
	}
	select {
	case <-def.done:
	case <-time.After(8 * time.Second):
		t.Fatal("client stopped reading and stalled the operation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := api.Drain(ctx); err != nil {
		t.Fatal(err)
	}
	operations, err := store.List(ctx, 1)
	if err != nil || len(operations) != 1 || operations[0].Status != state.Succeeded {
		t.Fatalf("stream failure prevented successful operation: %+v, %v", operations, err)
	}
}
