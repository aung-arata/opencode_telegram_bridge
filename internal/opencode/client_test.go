package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aung-arata/opencode-telegram-bridge/internal/logger"
)

// newTestClient creates a Client pointed at the given test server URL.
func newTestClient(serverURL string) *Client {
	return NewClient(serverURL, 5*time.Second, logger.New(""))
}

// sessionServer returns an httptest.Server whose POST /session handler returns
// sequential session IDs ("ses_1", "ses_2", …) and records call count.
func sessionServer(t *testing.T, callCount *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/session" {
			n := callCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(createSessionResponse{
				ID: "ses_" + itoa(n),
			})
			return
		}
		http.NotFound(w, r)
	}))
}

// itoa converts an int32 to its decimal string without importing strconv.
func itoa(n int32) string {
	if n == 0 {
		return "0"
	}
	buf := [10]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}

// TestGetOrCreateSession_CreatesSession verifies that the first call to
// GetOrCreateSession contacts the server and caches the returned session ID.
func TestGetOrCreateSession_CreatesSession(t *testing.T) {
	var calls atomic.Int32
	srv := sessionServer(t, &calls)
	defer srv.Close()

	c := newTestClient(srv.URL)
	ctx := context.Background()

	sid, err := c.GetOrCreateSession(ctx, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sid == "" {
		t.Fatal("expected a non-empty session ID")
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 server call, got %d", calls.Load())
	}
}

// TestGetOrCreateSession_ReusesSession verifies that calling GetOrCreateSession
// twice for the same chat hits the server only once.
func TestGetOrCreateSession_ReusesSession(t *testing.T) {
	var calls atomic.Int32
	srv := sessionServer(t, &calls)
	defer srv.Close()

	c := newTestClient(srv.URL)
	ctx := context.Background()

	sid1, err := c.GetOrCreateSession(ctx, 100)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	sid2, err := c.GetOrCreateSession(ctx, 100)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if sid1 != sid2 {
		t.Fatalf("expected same session ID, got %q and %q", sid1, sid2)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 server call, got %d", calls.Load())
	}
}

// TestResetSession_ClearsMapping verifies that ResetSession removes the cached
// session so the next GetOrCreateSession call creates a new one.
func TestResetSession_ClearsMapping(t *testing.T) {
	var calls atomic.Int32
	srv := sessionServer(t, &calls)
	defer srv.Close()

	c := newTestClient(srv.URL)
	ctx := context.Background()

	sid1, err := c.GetOrCreateSession(ctx, 100)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	c.ResetSession(100)

	sid2, err := c.GetOrCreateSession(ctx, 100)
	if err != nil {
		t.Fatalf("post-reset call: %v", err)
	}

	if sid1 == sid2 {
		t.Fatalf("expected a new session ID after reset, but got the same: %q", sid1)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 server calls (one before reset, one after), got %d", calls.Load())
	}
}

// TestResetSession_OtherChatUnaffected verifies that resetting one chat's
// session does not disturb another chat's cached session.
func TestResetSession_OtherChatUnaffected(t *testing.T) {
	var calls atomic.Int32
	srv := sessionServer(t, &calls)
	defer srv.Close()

	c := newTestClient(srv.URL)
	ctx := context.Background()

	sidA, _ := c.GetOrCreateSession(ctx, 1)
	sidB, _ := c.GetOrCreateSession(ctx, 2)

	c.ResetSession(1)

	sidBAfter, _ := c.GetOrCreateSession(ctx, 2)
	if sidB != sidBAfter {
		t.Fatalf("chat 2 session should be unchanged: want %q, got %q", sidB, sidBAfter)
	}

	sidAAfter, _ := c.GetOrCreateSession(ctx, 1)
	if sidA == sidAAfter {
		t.Fatalf("chat 1 session should be new after reset, but got the same: %q", sidA)
	}
}
