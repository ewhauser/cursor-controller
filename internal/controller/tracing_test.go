package controller

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ewhauser/cursor-controller/internal/backend"
	"github.com/ewhauser/cursor-controller/internal/cursorapi"
)

func TestSpansAreRecorded(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev); _ = tp.Shutdown(context.Background()) })

	api := &fakeAPI{
		listed: []cursorapi.PendingRequest{
			{ID: "r1", Labels: []cursorapi.Label{{Key: "pool", Value: "gpu"}}},
			{ID: "r2", ClaimedWorkerID: "cc-missing"},
		},
		cursor: "c0",
	}
	be := &fakeBackend{prefix: "cc", wakeErr: map[string]error{"cc-missing": backend.ErrWorkspaceMissing}}
	c := newTestController(api, be, Config{Pools: []string{"gpu"}})
	runUntilStream(t, c, api)
	if err := c.RunGC(context.Background()); err != nil {
		t.Fatal(err)
	}

	names := map[string]int{}
	results := map[string]string{}
	for _, s := range rec.Ended() {
		names[s.Name()]++
		for _, a := range s.Attributes() {
			if a.Key == attrResult {
				results[s.Name()] = a.Value.AsString()
			}
		}
	}
	if names["controller.claim_and_spawn"] != 1 || results["controller.claim_and_spawn"] != "spawned" {
		t.Errorf("claim span: count=%d result=%q", names["controller.claim_and_spawn"], results["controller.claim_and_spawn"])
	}
	if names["controller.wake"] != 1 || results["controller.wake"] != "workspace_missing" {
		t.Errorf("wake span: count=%d result=%q", names["controller.wake"], results["controller.wake"])
	}
	if names["controller.release"] != 1 || names["controller.gc"] != 1 {
		t.Errorf("release=%d gc=%d spans", names["controller.release"], names["controller.gc"])
	}
	// The release span must be a child of the wake span.
	var wakeID, releaseParent string
	for _, s := range rec.Ended() {
		switch s.Name() {
		case "controller.wake":
			wakeID = s.SpanContext().SpanID().String()
		case "controller.release":
			releaseParent = s.Parent().SpanID().String()
		}
	}
	if wakeID == "" || wakeID != releaseParent {
		t.Errorf("release parent %q != wake %q", releaseParent, wakeID)
	}
}
