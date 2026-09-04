package main

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/ewhauser/cursor-controller/internal/fakeapi"
)

func fakeapiNew(apiKey string, heartbeat, cursorTTL time.Duration, log *slog.Logger) http.Handler {
	return fakeapi.New(fakeapi.Options{APIKey: apiKey, Heartbeat: heartbeat, CursorTTL: cursorTTL, Log: log})
}
