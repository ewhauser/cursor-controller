// Command fake-cursor-api serves the in-memory fake of the Cursor fleet API
// for local integration and end-to-end testing.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := flag.String("addr", envOr("FAKE_API_ADDR", ":8080"), "listen address")
	apiKey := flag.String("api-key", os.Getenv("FAKE_API_KEY"), "API key clients must present (empty = no auth)")
	heartbeat := flag.Duration("heartbeat", 15*time.Second, "SSE heartbeat interval")
	cursorTTL := flag.Duration("cursor-ttl", 5*time.Minute, "how long list cursors stay valid")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv := fakeapiNew(*apiKey, *heartbeat, *cursorTTL, log)
	log.Info("fake-cursor-api listening", "addr", *addr, "auth", *apiKey != "")
	if err := http.ListenAndServe(*addr, srv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
