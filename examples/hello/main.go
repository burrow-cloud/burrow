// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Nicholas Phillips

// Command hello is a dead-simple HTTP server for the Burrow quickstart: it serves a single
// "Hello from Burrow" page on :8080 and a /healthz endpoint. It has no dependencies beyond the
// standard library so it builds into a tiny distroless image, and it is the app a stranger
// deploys to their local k3d cluster while following docs/QUICKSTART.md.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
)

// page is the HTML the root handler returns. It names the host so a browser hit visibly proves
// the request reached the pod (and, with more than one replica, which one answered).
const page = `<!doctype html>
<title>Hello from Burrow</title>
<body style="font-family: system-ui, sans-serif; max-width: 40rem; margin: 4rem auto; line-height: 1.5">
  <h1>Hello from Burrow 🐇</h1>
  <p>This app was built locally, imported into a k3d cluster, and deployed by Burrow.</p>
  <p>Served by pod <code>%s</code>.</p>
</body>
`

func main() {
	// The port is fixed at 8080 to match the quickstart's kubectl port-forward command; HOSTNAME is
	// the pod name Kubernetes injects, echoed into the page so the response is visibly from the pod.
	const addr = ":8080"
	host, _ := os.Hostname()

	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, page, host)
	})

	log.Printf("hello: listening on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("hello: %v", err)
	}
}
