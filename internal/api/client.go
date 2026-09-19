package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/kylebegeman/quark/internal/kernel"
	"github.com/kylebegeman/quark/internal/version"
)

// Client talks to a daemon.
type Client struct {
	http *http.Client
	base string
}

// ErrDisconnected means the daemon went away while an operation streamed.
// The operation itself is journaled and finishes on the daemon's side.
var ErrDisconnected = errors.New("lost the connection to the daemon")

// Dial returns a client for the daemon's Unix socket. It doesn't connect
// until the first call.
func Dial(socket string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{http: &http.Client{Transport: transport}, base: "http://quark"}
}

// NewClient returns a client for an HTTP base URL, for tests.
func NewClient(h *http.Client, base string) *Client { return &Client{http: h, base: base} }

// Reachable reports whether the daemon answers, within a short wait.
func (c *Client) Reachable(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := c.Version(ctx)
	return err == nil
}

// Version implements Runner.
func (c *Client) Version(ctx context.Context) (version.Info, error) {
	var info version.Info
	err := c.getJSON(ctx, "/v1/version", &info)
	return info, err
}

// List implements Runner.
func (c *Client) List(ctx context.Context, limit int) ([]Summary, error) {
	var rows []Summary
	err := c.getJSON(ctx, "/v1/operations?limit="+strconv.Itoa(limit), &rows)
	return rows, err
}

// Receipt implements Runner.
func (c *Client) Receipt(ctx context.Context, id string) (*kernel.Receipt, error) {
	var receipt kernel.Receipt
	if err := c.getJSON(ctx, "/v1/operations/"+id, &receipt); err != nil {
		return nil, err
	}
	return &receipt, nil
}

// Plan implements Runner.
func (c *Client) Plan(ctx context.Context, kind string, input json.RawMessage) (*kernel.PlanView, error) {
	var view *kernel.PlanView
	err := c.stream(ctx, RunRequest{Kind: kind, Input: input, PlanOnly: true}, func(ev kernel.Event) {
		if ev.Type == kernel.EventPlanned {
			view = ev.Plan
		}
	})
	if err != nil {
		return nil, err
	}
	if view == nil {
		return nil, errors.New("the daemon sent no plan")
	}
	return view, nil
}

// Run implements Runner. The receipt comes from the stream's final event.
// When the daemon disappears mid-way the error is ErrDisconnected and the
// operation's ID is in the events already seen.
func (c *Client) Run(ctx context.Context, kind string, input json.RawMessage, emit func(kernel.Event)) (*kernel.Receipt, error) {
	var receipt *kernel.Receipt
	err := c.stream(ctx, RunRequest{Kind: kind, Input: input}, func(ev kernel.Event) {
		if ev.Type == kernel.EventFinished {
			receipt = ev.Receipt
		}
		emit(ev)
	})
	if err != nil {
		return nil, err
	}
	if receipt == nil {
		return nil, ErrDisconnected
	}
	return receipt, nil
}

// Close implements Runner.
func (c *Client) Close() error {
	c.http.CloseIdleConnections()
	return nil
}

func (c *Client) getJSON(ctx context.Context, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

func (c *Client) stream(ctx context.Context, req RunRequest, each func(kernel.Event)) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/operations", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scanner.Scan() {
		var ev kernel.Event
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			return fmt.Errorf("bad event from the daemon: %w", err)
		}
		if ev.Type == kernel.EventError {
			return errors.New(ev.Message)
		}
		each(ev)
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || ctx.Err() == nil {
			return fmt.Errorf("%w: %v", ErrDisconnected, err)
		}
		return err
	}
	return nil
}

func apiError(resp *http.Response) error {
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err == nil && body.Error != "" {
		return errors.New(body.Error)
	}
	return fmt.Errorf("daemon answered %s", resp.Status)
}
