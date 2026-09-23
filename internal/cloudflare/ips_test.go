package cloudflare_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/kylebegeman/bedrock/internal/cloudflare"
	"github.com/kylebegeman/bedrock/internal/cloudflare/cloudflaretest"
)

// The ranges are public, so asking needs no token and sends none.
func TestTheProxysRangesAreReadWithoutAToken(t *testing.T) {
	fake := cloudflaretest.New("t0ken")
	fake.IPv4 = []string{"173.245.48.0/20"}
	fake.IPv6 = []string{"2400:cb00::/32"}
	var sawAuth atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuth.Store(true)
		}
		fake.ServeHTTP(w, r)
	}))
	defer srv.Close()
	c := cloudflare.New("")
	c.Base = srv.URL + "/client/v4"
	got, err := c.IPs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.All(), []string{"173.245.48.0/20", "2400:cb00::/32"}) || sawAuth.Load() {
		t.Fatalf("%+v, authorization sent: %v", got, sawAuth.Load())
	}
	fake.IPv4 = nil
	if _, err := c.IPs(context.Background()); err == nil {
		t.Fatal("a list with no IPv4 ranges must be refused")
	}
}
