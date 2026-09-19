package edge

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// AdminAddress is where the edge's admin API answers on the machine.
const AdminAddress = "http://127.0.0.1:2019"

// Admin talks to Caddy's admin API.
type Admin struct {
	Base string
	http *http.Client
}

// NewAdmin returns a client for the edge on this machine.
func NewAdmin() *Admin {
	return &Admin{Base: AdminAddress, http: &http.Client{Timeout: 15 * time.Second}}
}

// Load replaces the whole configuration atomically.
func (a *Admin) Load(ctx context.Context, config []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Base+"/load", bytes.NewReader(config))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("edge admin: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("edge rejected the configuration: %s: %s", resp.Status, bytes.TrimSpace(body))
	}
	return nil
}

// Current returns the configuration the edge is running.
func (a *Admin) Current(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.Base+"/config/", nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("edge admin: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("edge admin answered %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// Answers reports whether the admin API is up.
func (a *Admin) Answers(ctx context.Context) bool {
	_, err := a.Current(ctx)
	return err == nil
}
