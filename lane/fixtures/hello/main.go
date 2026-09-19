// A tiny web app for the lane: it says hello and reports which revision
// answers, so a deploy and a rollback are visible from outside.
package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	revision := os.Getenv("QUARK_REVISION")
	if revision == "" {
		revision = "unknown"
	}
	greeting := os.Getenv("GREETING")
	if greeting == "" {
		greeting = "hello"
	}
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s from quark, revision %s, path %s\n", greeting, revision, r.URL.Path)
	})
	if err := http.ListenAndServe(":8000", nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
