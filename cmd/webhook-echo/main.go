// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// webhook-echo is a tiny HTTP server that prints every incoming
// webhook to stdout. Use it to receive Squadron's v0.33
// silent-agent webhooks and v0.35 deploy-completion webhooks during
// local testing — saves you from trusting webhook.site or
// equivalent for sensitive payloads.
//
// Usage:
//
//	webhook-echo [-addr :9001]
//
// In Squadron's config:
//
//	silent_agents:
//	  webhook_url: http://localhost:9001/silent
//	deploy:
//	  completion_webhook_url: http://localhost:9001/deploy
//
// (Or when Squadron is running in docker-compose, use
// http://host.docker.internal:9001/...)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", ":9001", "listen address")
	flag.Parse()

	http.HandleFunc("/", handler)
	log.Printf("webhook-echo listening on %s", *addr)
	log.Printf("send anything to http://localhost%s/silent or /deploy or any path", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// handler prints the incoming request in a human-readable form
// with a clear visual separator so successive webhooks stay
// scannable in a terminal.
func handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()

	stamp := time.Now().Format("15:04:05.000")
	separator := strings.Repeat("─", 70)

	fmt.Println()
	fmt.Println(separator)
	fmt.Printf("  [%s] %s %s\n", stamp, r.Method, r.URL.Path)
	fmt.Println(separator)

	// Headers worth printing: User-Agent + Content-Type tell you
	// which Squadron subsystem sent the payload.
	for _, k := range []string{"User-Agent", "Content-Type"} {
		if v := r.Header.Get(k); v != "" {
			fmt.Printf("  %s: %s\n", k, v)
		}
	}

	// Pretty-print JSON when possible; fall back to raw bytes.
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var pretty any
		if err := json.Unmarshal(body, &pretty); err == nil {
			out, _ := json.MarshalIndent(pretty, "  ", "  ")
			fmt.Println()
			fmt.Print("  " + string(out) + "\n")
		} else {
			fmt.Printf("\n  (not valid JSON; raw): %s\n", string(body))
		}
	} else {
		fmt.Printf("\n  %s\n", string(body))
	}

	// Always 200 OK so Squadron treats the delivery as successful
	// (we don't want to trigger retries during testing).
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}
