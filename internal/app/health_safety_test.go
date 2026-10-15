package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kylebegeman/helios/internal/docker"
	"github.com/kylebegeman/helios/internal/docker/dockertest"
	"github.com/kylebegeman/helios/internal/manifest"
)

func TestHTTPReadinessProbeStopsWhenItsContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())
	done := make(chan bool, 1)
	go func() {
		ok, _ := probeReady(ctx, nil, "container", &docker.Info{IPs: map[string]string{"private": u.Hostname()}}, manifest.Workload{Port: port, Health: &manifest.Health{Path: "/ready"}})
		done <- ok
	}()
	<-entered
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("cancelled readiness succeeded")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("readiness request ignored operation cancellation")
	}
}

func TestVerificationRejectsATruncatedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.Write([]byte("ready"))
	}))
	defer server.Close()
	check := manifest.Check{URL: server.URL, Contains: "ready"}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, check.URL, nil)
	if err := evaluateCheck(check, server.Client(), req, time.Second); err == nil || !strings.Contains(err.Error(), "response") {
		t.Fatalf("truncated response accepted: %v", err)
	}
}

func TestReadinessTimeoutBoundsAnInFlightProbe(t *testing.T) {
	fake := dockertest.New(t)
	entered := make(chan struct{})
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v"+dockertest.APIVersion)
		if path == "/containers/slow-ready/json" {
			json.NewEncoder(w).Encode(map[string]any{"Id": "slow-id", "Name": "/slow-ready", "State": map[string]any{"Running": true}, "Config": map[string]any{}, "NetworkSettings": map[string]any{"Networks": map[string]any{"private": map[string]string{"IPAddress": "127.0.0.1"}}}})
			return
		}
		if path == "/ready" {
			close(entered)
			<-r.Context().Done()
			return
		}
		fake.ServeHTTP(w, r)
	}))
	defer proxy.Close()
	t.Setenv("DOCKER_HOST", "tcp"+strings.TrimPrefix(proxy.URL, "http"))
	u, _ := url.Parse(proxy.URL)
	port, _ := strconv.Atoi(u.Port())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine, err := docker.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	done := make(chan error, 1)
	go func() {
		done <- waitReady(ctx, engine, "slow-ready", manifest.Workload{Port: port, Health: &manifest.Health{Path: "/ready", Timeout: "40ms"}})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("readiness probe did not start")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("timed out probe was ready")
		}
	case <-time.After(250 * time.Millisecond):
		cancel()
		<-done
		t.Fatal("in-flight probe exceeded the configured readiness timeout")
	}
}
