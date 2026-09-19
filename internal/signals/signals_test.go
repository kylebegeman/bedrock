package signals

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/kylebegeman/quark/internal/state"
)

const scrape1 = `# HELP caddy_http_requests_total Counter of HTTP(S) requests made.
# TYPE caddy_http_requests_total counter
caddy_http_requests_total{handler="subroute",host="hello.lane.begam.in",server="quark"} 10
caddy_http_request_duration_seconds_count{code="200",handler="subroute",host="hello.lane.begam.in",method="GET",server="quark"} 8
caddy_http_request_duration_seconds_count{code="502",handler="subroute",host="hello.lane.begam.in",method="GET",server="quark"} 2
caddy_http_request_duration_seconds_sum{code="200",handler="subroute",host="hello.lane.begam.in",method="GET",server="quark"} 0.4
caddy_http_request_duration_seconds_bucket{code="200",handler="subroute",host="hello.lane.begam.in",method="GET",server="quark",le="0.05"} 6
caddy_http_request_duration_seconds_bucket{code="200",handler="subroute",host="hello.lane.begam.in",method="GET",server="quark",le="0.25"} 8
caddy_http_request_duration_seconds_bucket{code="200",handler="subroute",host="hello.lane.begam.in",method="GET",server="quark",le="+Inf"} 8
caddy_http_response_size_bytes_sum{code="200",handler="subroute",host="hello.lane.begam.in",method="GET",server="quark"} 800
caddy_http_requests_total{handler="subroute",host="nobody.example",server="quark"} 99
caddy_http_requests_total{handler="reverse_proxy",host="hello.lane.begam.in",server="quark"} 1000
`

const scrape2 = `caddy_http_requests_total{handler="subroute",host="hello.lane.begam.in",server="quark"} 25
caddy_http_request_duration_seconds_count{code="200",handler="subroute",host="hello.lane.begam.in",method="GET",server="quark"} 22
caddy_http_request_duration_seconds_count{code="502",handler="subroute",host="hello.lane.begam.in",method="GET",server="quark"} 3
caddy_http_request_duration_seconds_count{code="503",handler="subroute",host="hello.lane.begam.in",method="POST",server="quark"} 2
caddy_http_request_duration_seconds_sum{code="200",handler="subroute",host="hello.lane.begam.in",method="GET",server="quark"} 1.9
caddy_http_request_duration_seconds_bucket{code="200",handler="subroute",host="hello.lane.begam.in",method="GET",server="quark",le="0.05"} 16
caddy_http_request_duration_seconds_bucket{code="200",handler="subroute",host="hello.lane.begam.in",method="GET",server="quark",le="0.25"} 22
caddy_http_request_duration_seconds_bucket{code="200",handler="subroute",host="hello.lane.begam.in",method="GET",server="quark",le="+Inf"} 22
caddy_http_response_size_bytes_sum{code="200",handler="subroute",host="hello.lane.begam.in",method="GET",server="quark"} 2000
`

func TestParsesPrometheusText(t *testing.T) {
	series := ParseMetrics(scrape1)
	if len(series) != 10 {
		t.Fatalf("parsed %d series", len(series))
	}
	first := series[0]
	if first.Name != "caddy_http_requests_total" || first.Labels["host"] != "hello.lane.begam.in" || first.Value != 10 {
		t.Fatalf("%+v", first)
	}
	if series[4].Labels["le"] != "0.05" || series[4].Value != 6 {
		t.Fatalf("%+v", series[4])
	}
}

func TestTrafficRollsUpDeltasAfterPriming(t *testing.T) {
	ctx := context.Background()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 9, 19, 16, 30, 0, 0, time.UTC)
	s := &Sampler{Store: store, Log: func(string, ...any) {}}
	hosts := map[string]string{"hello.lane.begam.in": "hello"}
	if err := s.traffic(ctx, now, ParseMetrics(scrape1), hosts); err != nil {
		t.Fatal(err)
	}
	if rows, _ := store.Signals(ctx, "hello", now.Add(-time.Hour)); len(rows) != 0 {
		t.Fatalf("the first round must only prime: %+v", rows)
	}
	if err := s.traffic(ctx, now.Add(time.Minute), ParseMetrics(scrape2), hosts); err != nil {
		t.Fatal(err)
	}
	rows, _ := store.Signals(ctx, "hello", now.Add(-time.Hour))
	sum := Summarize(rows)
	// The 503 series appeared after priming, so it counts in full.
	if sum.Requests != 15 || sum.Errors != 3 || sum.Bytes != 1200 || sum.Hours != 1 {
		t.Fatalf("%+v", sum)
	}
	// 1.5 s over the 17 responses the histogram saw; 14 finished within
	// 0.25 s, 10 within 0.05 s, so the 95th percentile falls to the last bucket.
	if math.Abs(sum.AvgMS-1500.0/17) > 0.01 || sum.P95MS != 250 {
		t.Fatalf("latency: %+v", sum)
	}
	// The edge restarts: counters fall back to small numbers and count from zero.
	if err := s.traffic(ctx, now.Add(2*time.Minute), ParseMetrics(scrape1), hosts); err != nil {
		t.Fatal(err)
	}
	rows, _ = store.Signals(ctx, "hello", now.Add(-time.Hour))
	if sum := Summarize(rows); sum.Requests != 25 {
		t.Fatalf("after a reset: %+v", sum)
	}
	if rows, _ := store.Signals(ctx, "nobody", now.Add(-time.Hour)); len(rows) != 0 {
		t.Fatal("an unknown host was rolled up")
	}
}

func TestSummarizeAveragesResourcesPerWorkload(t *testing.T) {
	hour := time.Date(2026, 9, 19, 16, 0, 0, 0, time.UTC)
	rows := []state.Signal{
		{Hour: hour, App: "a", Key: "web", Metric: state.SignalCPUPercent, Value: 30, Samples: 3},
		{Hour: hour, App: "a", Key: "worker", Metric: state.SignalCPUPercent, Value: 5, Samples: 1},
		{Hour: hour, App: "a", Key: "web", Metric: state.SignalMemoryBytes, Value: 300, Samples: 3},
		{Hour: hour, App: "a", Key: "web", Metric: state.SignalMemoryMax, Value: 150},
		{Hour: hour.Add(-time.Hour), App: "a", Key: "", Metric: state.SignalDiskBytes, Value: 5000},
		{Hour: hour, App: "a", Key: "", Metric: state.SignalDiskBytes, Value: 7000},
	}
	sum := Summarize(rows)
	if sum.CPUPercent != 15 || sum.MemoryBytes != 100 || sum.MemoryMax != 150 || sum.DiskBytes != 7000 || sum.Hours != 2 {
		t.Fatalf("%+v", sum)
	}
}
