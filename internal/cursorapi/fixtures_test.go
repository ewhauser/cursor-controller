package cursorapi

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Fixtures captured with hack/capture-fixtures.sh from the real API. Each test
// skips when its file is absent so CI passes without credentials.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Skipf("no fixture %s (run hack/capture-fixtures.sh)", name)
	}
	return b
}

func TestFixturePendingRequests(t *testing.T) {
	var page PendingRequestsPage
	if err := json.Unmarshal(fixture(t, "pending-requests.json"), &page); err != nil {
		t.Fatal(err)
	}
	if page.StreamCursor == "" {
		t.Error("streamCursor missing from list response")
	}
	for _, r := range page.Requests {
		if r.ID == "" {
			t.Errorf("request without id: %+v", r)
		}
		t.Logf("request %s pool=%s repo=%s claimedWorker=%q wake=%s", r.ID, r.Pool(), r.RepoURL, r.ClaimedWorkerID, r.WakeTimeout())
	}
}

func TestFixtureStream(t *testing.T) {
	raw := fixture(t, "stream.sse")
	var n int
	err := ParseSSE(strings.NewReader(string(raw)), func(e Event) error {
		n++
		switch e.Type {
		case EventCreated, EventClaimedOffline:
			r, err := DecodeRequest(e.Data)
			if err != nil {
				t.Errorf("%s payload: %v: %s", e.Type, err, e.Data)
			} else if e.Type == EventClaimedOffline && r.ClaimedWorkerID == "" {
				t.Errorf("claimed_offline without claimedWorkerId: %s", e.Data)
			}
		case EventClaimed:
			if _, err := DecodeClaimEvent(e.Data); err != nil {
				t.Errorf("claimed payload: %v: %s", err, e.Data)
			}
		}
		if e.ID == "" && e.Type != "" {
			t.Errorf("event %s has no id (resume cursor)", e.Type)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("parsed %d events", n)
}

func TestFixtureAgent(t *testing.T) {
	var a Agent
	if err := json.Unmarshal(fixture(t, "agent.json"), &a); err != nil {
		t.Fatal(err)
	}
	switch a.Status {
	case AgentStatusActive, AgentStatusIdle, AgentStatusArchived:
	default:
		t.Errorf("unexpected agent status %q", a.Status)
	}
}
