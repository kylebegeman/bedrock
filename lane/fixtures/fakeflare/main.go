// Fakeflare stands in for Cloudflare's API on the lane: zones and DNS
// records, kept in memory, behind a token. The M6 proof points bedrock's
// cloudflare integration at it so no real token is needed; the records it
// keeps don't change public DNS (the lane's wildcard does that).
//
//	FAKEFLARE_TOKEN=... FAKEFLARE_ZONES=example.com fakeflare -listen :8788
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/kylebegeman/bedrock/internal/cloudflare/cloudflaretest"
)

func main() {
	listen := flag.String("listen", ":8788", "address to listen on")
	flag.Parse()
	token := os.Getenv("FAKEFLARE_TOKEN")
	if token == "" {
		log.Fatal("FAKEFLARE_TOKEN is required")
	}
	zones := strings.Split(os.Getenv("FAKEFLARE_ZONES"), ",")
	srv := cloudflaretest.New(token, zones...)
	log.Printf("fakeflare on %s for %s", *listen, strings.Join(zones, ", "))
	log.Fatal(http.ListenAndServe(*listen, srv))
}
