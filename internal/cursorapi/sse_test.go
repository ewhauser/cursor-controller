package cursorapi

import (
	"strings"
	"testing"
)

func TestParseSSE(t *testing.T) {
	body := ": connected\r\n\r\nevent: created\nid: 5\ndata: {\"id\":\"x\"}\n\nevent: heartbeat\r\nid: 6\r\ndata: {}\r\n\r\ndata: line1\ndata: line2\n\nevent: claimed\nid: 7\ndata: {\"id\":\"y\"}"
	var got []Event
	err := ParseSSE(strings.NewReader(body), func(e Event) error { got = append(got, e); return nil })
	if err != nil {
		t.Fatal(err)
	}
	want := []Event{
		{Type: "created", ID: "5", Data: `{"id":"x"}`},
		{Type: "heartbeat", ID: "6", Data: "{}"},
		{Data: "line1\nline2"},
		{Type: "claimed", ID: "7", Data: `{"id":"y"}`},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}

func TestPendingRequestPool(t *testing.T) {
	r := PendingRequest{Labels: []Label{{Key: "team", Value: "x"}}}
	if r.Pool() != "default" {
		t.Fatalf("pool = %q", r.Pool())
	}
	r.Labels = append(r.Labels, Label{Key: "pool", Value: "gpu"})
	if r.Pool() != "gpu" {
		t.Fatalf("pool = %q", r.Pool())
	}
}
