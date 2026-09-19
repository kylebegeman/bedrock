// A small app shaped like Loom's Core, for the lane. The API answers on
// two ports, and its health reaches the database its derived URL names;
// the worker is a singleton whose health goes through the API's alias.
// One binary, one image, several workloads.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "worker":
			worker()
			return
		case "check":
			check(os.Args[2])
			return
		}
	}
	serve()
}

func serve() {
	revision := os.Getenv("QUARK_REVISION")
	health := func(w http.ResponseWriter, _ *http.Request) {
		// Ready when the database in the derived URL takes connections.
		if raw := os.Getenv("NOTES_DATABASE_URL"); raw != "" {
			u, err := url.Parse(raw)
			if err != nil {
				http.Error(w, "bad database URL", http.StatusServiceUnavailable)
				return
			}
			conn, err := net.DialTimeout("tcp", u.Host, 3*time.Second)
			if err != nil {
				http.Error(w, "database unreachable: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
			conn.Close()
		}
		fmt.Fprintln(w, "ok")
	}
	api := http.NewServeMux()
	api.HandleFunc("/healthz", health)
	api.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "notes api, revision %s, note %s\n", revision, os.Getenv("NOTE"))
	})
	control := http.NewServeMux()
	control.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "notes control, revision %s\n", revision)
	})
	go func() {
		if err := http.ListenAndServe(":8001", control); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}()
	if err := http.ListenAndServe(":8000", api); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func worker() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Println("worker up, revision", os.Getenv("QUARK_REVISION"))
	<-ctx.Done()
	fmt.Println("worker stopping")
}

func check(target string) {
	if os.Getenv("FAIL") == "1" {
		fmt.Println("told to fail")
		os.Exit(1)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(target)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Println(target, "answered", resp.Status)
		os.Exit(1)
	}
}
