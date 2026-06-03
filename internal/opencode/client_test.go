package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aung-arata/opencode-telegram-bridge/internal/logger"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// newTestClient creates a Client pointed at the given test server URL.
func newTestClient(serverURL string) *Client {
	return NewClient(serverURL, 5*time.Second, logger.New(""), "")
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

// globalEventJSON builds a /global/event SSE data payload.
func globalEventJSON(t *testing.T, evType string, props any) string {
	t.Helper()
	p, err := json.Marshal(props)
	if err != nil {
		t.Fatalf("marshal props: %v", err)
	}
	ge := globalEvent{Payload: globalPayload{Type: evType, Properties: json.RawMessage(p)}}
	b, err := json.Marshal(ge)
	if err != nil {
		t.Fatalf("marshal globalEvent: %v", err)
	}
	return string(b)
}

// sseLines formats SSE lines for a single event: optional event field, one
// data line, and the terminating blank line.
func sseLines(event, data string) string {
	var sb strings.Builder
	if event != "" {
		fmt.Fprintf(&sb, "event: %s\n", event)
	}
	fmt.Fprintf(&sb, "data: %s\n\n", data)
	return sb.String()
}

// ---------------------------------------------------------------------------
// CreateSession
// ---------------------------------------------------------------------------

func TestCreateSession_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/session" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(createSessionResponse{ID: "ses_abc"})
	}))
	defer srv.Close()

	sid, err := newTestClient(srv.URL).CreateSession(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sid != "ses_abc" {
		t.Fatalf("want ses_abc, got %q", sid)
	}
}

func TestCreateSession_SendsAgentField(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(createSessionResponse{ID: "ses_plan"})
	}))
	defer srv.Close()

	_, err := newTestClient(srv.URL).CreateSession(context.Background(), "plan")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, _ := gotBody["agent"].(string); got != "plan" {
		t.Fatalf("want agent=plan in body, got %v", gotBody)
	}
}

func TestCreateSession_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := newTestClient(srv.URL).CreateSession(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
}

func TestCreateSession_EmptyID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(createSessionResponse{ID: ""})
	}))
	defer srv.Close()

	_, err := newTestClient(srv.URL).CreateSession(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty session ID")
	}
}

func TestSwitchMode_ReplacesSession(t *testing.T) {
	var calls atomic.Int32
	srv := sessionServer(t, &calls)
	defer srv.Close()

	c := newTestClient(srv.URL)
	ctx := context.Background()

	// Create an initial session.
	sid1, _ := c.GetOrCreateSession(ctx, 42)

	// Switch mode — should replace the cached session.
	sid2, err := c.SwitchMode(ctx, 42, "plan")
	if err != nil {
		t.Fatalf("SwitchMode error: %v", err)
	}
	if sid1 == sid2 {
		t.Fatal("expected a new session ID after SwitchMode")
	}

	// GetSession should now return sid2.
	got, ok := c.GetSession(42)
	if !ok || got != sid2 {
		t.Fatalf("want %q, got %q (ok=%v)", sid2, got, ok)
	}
}

// ---------------------------------------------------------------------------
// SendMessage
// ---------------------------------------------------------------------------

func TestSendMessage_Success(t *testing.T) {
	var gotBody sendMessageRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/session/ses_1/message" {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := newTestClient(srv.URL).SendMessage(context.Background(), "ses_1", "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotBody.Parts) == 0 || gotBody.Parts[0].Text != "hello" {
		t.Fatalf("unexpected body: %+v", gotBody)
	}
}

func TestSendMessage_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	err := newTestClient(srv.URL).SendMessage(context.Background(), "ses_1", "hi")
	if err == nil {
		t.Fatal("expected error for HTTP 404")
	}
}

// ---------------------------------------------------------------------------
// Close
// ---------------------------------------------------------------------------

func TestClose_ClearsAllSessions(t *testing.T) {
	var calls atomic.Int32
	srv := sessionServer(t, &calls)
	defer srv.Close()

	c := newTestClient(srv.URL)
	ctx := context.Background()

	c.GetOrCreateSession(ctx, 1)
	c.GetOrCreateSession(ctx, 2)
	c.Close()

	// After Close both chats should create new sessions.
	c.GetOrCreateSession(ctx, 1)
	c.GetOrCreateSession(ctx, 2)

	if calls.Load() != 4 {
		t.Fatalf("expected 4 server calls after Close, got %d", calls.Load())
	}
}

// ---------------------------------------------------------------------------
// GetOrCreateSession
// ---------------------------------------------------------------------------

func TestGetOrCreateSession_CreatesSession(t *testing.T) {
	var calls atomic.Int32
	srv := sessionServer(t, &calls)
	defer srv.Close()

	c := newTestClient(srv.URL)
	sid, err := c.GetOrCreateSession(context.Background(), 100)
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

func TestGetOrCreateSession_ReusesSession(t *testing.T) {
	var calls atomic.Int32
	srv := sessionServer(t, &calls)
	defer srv.Close()

	c := newTestClient(srv.URL)
	ctx := context.Background()

	sid1, _ := c.GetOrCreateSession(ctx, 100)
	sid2, _ := c.GetOrCreateSession(ctx, 100)

	if sid1 != sid2 {
		t.Fatalf("expected same session ID, got %q and %q", sid1, sid2)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 server call, got %d", calls.Load())
	}
}

// ---------------------------------------------------------------------------
// ResetSession
// ---------------------------------------------------------------------------

func TestResetSession_ClearsMapping(t *testing.T) {
	var calls atomic.Int32
	srv := sessionServer(t, &calls)
	defer srv.Close()

	c := newTestClient(srv.URL)
	ctx := context.Background()

	sid1, _ := c.GetOrCreateSession(ctx, 100)
	c.ResetSession(100)
	sid2, err := c.GetOrCreateSession(ctx, 100)
	if err != nil {
		t.Fatalf("post-reset: %v", err)
	}
	if sid1 == sid2 {
		t.Fatalf("expected new session ID after reset, got same: %q", sid1)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 server calls, got %d", calls.Load())
	}
}

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
		t.Fatalf("chat 1 should have new session after reset, got same: %q", sidA)
	}
}

func TestGetSession_ReturnsCachedSession(t *testing.T) {
	var calls atomic.Int32
	srv := sessionServer(t, &calls)
	defer srv.Close()

	c := newTestClient(srv.URL)
	ctx := context.Background()

	// Before any session exists.
	_, ok := c.GetSession(42)
	if ok {
		t.Fatal("expected no session before any query")
	}

	sid, _ := c.GetOrCreateSession(ctx, 42)

	got, ok := c.GetSession(42)
	if !ok {
		t.Fatal("expected session to exist after GetOrCreateSession")
	}
	if got != sid {
		t.Fatalf("want %q, got %q", sid, got)
	}

	// After reset, should be gone again.
	c.ResetSession(42)
	_, ok = c.GetSession(42)
	if ok {
		t.Fatal("expected no session after ResetSession")
	}
}

func TestAbort_SendsCorrectRequest(t *testing.T) {
	var abortPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/abort") {
			abortPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("true"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	err := c.Abort(context.Background(), "ses_test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if abortPath != "/session/ses_test/abort" {
		t.Fatalf("want path /session/ses_test/abort, got %q", abortPath)
	}
}

func TestAbort_ErrorOnNonOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "session not found", http.StatusNotFound)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	err := c.Abort(context.Background(), "ses_missing")
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
}

// ---------------------------------------------------------------------------
// extractText
// ---------------------------------------------------------------------------

func TestExtractText(t *testing.T) {
	cases := []struct {
		name    string
		data    string
		deltaOK bool
		delta   contentDelta
		want    string
	}{
		{"empty data", "", true, contentDelta{}, ""},
		{"DONE sentinel", "[DONE]", true, contentDelta{}, ""},
		{"content field", "x", true, contentDelta{Content: "hello"}, "hello"},
		{"text field", "x", true, contentDelta{Text: "world"}, "world"},
		{"delta field", "x", true, contentDelta{Delta: "chunk"}, "chunk"},
		{"parts text", "x", true, contentDelta{Parts: []contentPart{{Type: "text", Text: "abc"}}}, "abc"},
		{"parts content fallback", "x", true, contentDelta{Parts: []contentPart{{Type: "text", Content: "def"}}}, "def"},
		{"parts non-text skipped", "x", true, contentDelta{Parts: []contentPart{{Type: "image", Text: "skip"}}}, ""},
		{"not json — raw data returned", "raw text", false, contentDelta{}, "raw text"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractText(tc.data, tc.deltaOK, &tc.delta)
			if got != tc.want {
				t.Fatalf("want %q, got %q", tc.want, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// isCompletionEvent
// ---------------------------------------------------------------------------

func TestIsCompletionEvent(t *testing.T) {
	noDelta := contentDelta{}

	cases := []struct {
		name      string
		eventName string
		data      string
		deltaOK   bool
		delta     contentDelta
		want      bool
	}{
		{"[DONE] data", "", "[DONE]", false, noDelta, true},
		{"event=done", "done", "x", false, noDelta, true},
		{"event=complete", "complete", "x", false, noDelta, true},
		{"event=session.idle", "session.idle", "x", false, noDelta, true},
		{"event=message_stop", "message_stop", "x", false, noDelta, true},
		{"event=finish", "finish", "x", false, noDelta, true},
		{"parts step-finish", "", "x", true, contentDelta{Parts: []contentPart{{Type: "step-finish"}}}, true},
		{"parts message-finish", "", "x", true, contentDelta{Parts: []contentPart{{Type: "message-finish"}}}, true},
		{"not completion", "data", "hello", true, noDelta, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isCompletionEvent(tc.eventName, tc.data, tc.deltaOK, &tc.delta)
			if got != tc.want {
				t.Fatalf("want %v, got %v", tc.want, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// readSSE
// ---------------------------------------------------------------------------

func TestReadSSE_GlobalEvent_TextAndIdle(t *testing.T) {
	sid := "ses_test"
	chunk1 := globalEventJSON(t, "message.part.delta", partDeltaProperties{SessionID: sid, Field: "text", Delta: "Hello "})
	chunk2 := globalEventJSON(t, "message.part.delta", partDeltaProperties{SessionID: sid, Field: "text", Delta: "world"})
	idle := globalEventJSON(t, "session.idle", sessionEventProperties{SessionID: sid})

	raw := sseLines("", chunk1) + sseLines("", chunk2) + sseLines("", idle)

	var chunks []string
	c := newTestClient("http://unused")
	got, err := c.readSSE(strings.NewReader(raw), sid, func(acc string) { chunks = append(chunks, acc) }, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Hello world" {
		t.Fatalf("want %q, got %q", "Hello world", got)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunk callbacks, got %d", len(chunks))
	}
}

func TestReadSSE_SkipsOtherSession(t *testing.T) {
	sid := "ses_mine"
	other := globalEventJSON(t, "message.part.delta", partDeltaProperties{SessionID: "ses_other", Field: "text", Delta: "noise"})
	mine := globalEventJSON(t, "message.part.delta", partDeltaProperties{SessionID: sid, Field: "text", Delta: "signal"})
	idle := globalEventJSON(t, "session.idle", sessionEventProperties{SessionID: sid})

	raw := sseLines("", other) + sseLines("", mine) + sseLines("", idle)

	c := newTestClient("http://unused")
	got, err := c.readSSE(strings.NewReader(raw), sid, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "signal" {
		t.Fatalf("want %q, got %q", "signal", got)
	}
}

func TestReadSSE_LegacySSE_CompletionByEvent(t *testing.T) {
	// Non-global-format SSE: plain text delta followed by a "done" event.
	raw := sseLines("", `{"content":"legacy text"}`) + sseLines("done", "[DONE]")

	c := newTestClient("http://unused")
	got, err := c.readSSE(strings.NewReader(raw), "ses_x", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "legacy text" {
		t.Fatalf("want %q, got %q", "legacy text", got)
	}
}

func TestReadSSE_EOFWithContent(t *testing.T) {
	// Stream closes without a completion event; accumulated text should still be returned.
	raw := sseLines("", `{"content":"partial"}`)

	c := newTestClient("http://unused")
	got, err := c.readSSE(strings.NewReader(raw), "ses_x", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "partial" {
		t.Fatalf("want %q, got %q", "partial", got)
	}
}

func TestReadSSE_EmptyStream(t *testing.T) {
	c := newTestClient("http://unused")
	got, err := c.readSSE(strings.NewReader(""), "ses_x", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

func TestReadSSE_PermissionAsked_CallbackInvoked(t *testing.T) {
	sid := "ses_perm"
	permEvent := globalEventJSON(t, "permission.asked", PermissionRequest{
		ID:         "per_abc",
		SessionID:  sid,
		Permission: "bash",
		Patterns:   []string{"ls /tmp"},
		Always:     []string{},
		Metadata:   map[string]any{},
	})
	text := globalEventJSON(t, "message.part.delta", partDeltaProperties{SessionID: sid, Field: "text", Delta: "done"})
	idle := globalEventJSON(t, "session.idle", sessionEventProperties{SessionID: sid})

	raw := sseLines("", permEvent) + sseLines("", text) + sseLines("", idle)

	var got PermissionRequest
	c := newTestClient("http://unused")
	result, err := c.readSSE(strings.NewReader(raw), sid, nil, func(req PermissionRequest) {
		got = req
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "done" {
		t.Fatalf("want %q, got %q", "done", result)
	}
	if got.ID != "per_abc" {
		t.Fatalf("want permissionID %q, got %q", "per_abc", got.ID)
	}
	if got.Permission != "bash" {
		t.Fatalf("want permission %q, got %q", "bash", got.Permission)
	}
}

func TestRespondToPermission_OK(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/session/ses_x/permissions/per_y" {
			gotBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("null"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	err := c.RespondToPermission(context.Background(), "ses_x", "per_y", "once")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var body struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(gotBody, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if body.Response != "once" {
		t.Fatalf("want response %q, got %q", "once", body.Response)
	}
}

// ---------------------------------------------------------------------------
// Query (full round-trip)
// ---------------------------------------------------------------------------

// buildQueryServer creates an httptest.Server that implements the three
// endpoints Query depends on: POST /session, POST /session/{id}/message,
// and GET /global/event (SSE stream).
//
// The SSE stream emits the provided text as a single delta then sends
// session.idle to signal completion.
func buildQueryServer(t *testing.T, responseText string) *httptest.Server {
	t.Helper()
	var sessionCalls atomic.Int32

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			n := sessionCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(createSessionResponse{ID: "ses_" + itoa(n)})

		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/session/") && strings.HasSuffix(r.URL.Path, "/message"):
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && r.URL.Path == "/global/event":
			// Determine which session this SSE connection belongs to by waiting
			// for the message to arrive (handled concurrently); since we control
			// both goroutines here we simply emit for ses_1.
			sid := "ses_" + itoa(sessionCalls.Load())
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.(http.Flusher).Flush()

			deltaProps := partDeltaProperties{SessionID: sid, Field: "text", Delta: responseText}
			idleProps := sessionEventProperties{SessionID: sid}

			fmt.Fprintf(w, "data: %s\n\n", globalEventJSON(t, "message.part.delta", deltaProps))
			w.(http.Flusher).Flush()
			fmt.Fprintf(w, "data: %s\n\n", globalEventJSON(t, "session.idle", idleProps))
			w.(http.Flusher).Flush()

		default:
			http.NotFound(w, r)
		}
	}))
}

func TestQuery_FullRoundTrip(t *testing.T) {
	srv := buildQueryServer(t, "the answer")
	defer srv.Close()

	c := newTestClient(srv.URL)
	got, err := c.Query(context.Background(), 42, "what is the answer?", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "the answer" {
		t.Fatalf("want %q, got %q", "the answer", got)
	}
}

func TestQuery_StreamingCallbacks(t *testing.T) {
	srv := buildQueryServer(t, "streamed")
	defer srv.Close()

	c := newTestClient(srv.URL)
	var chunks []string
	c.Query(context.Background(), 42, "stream test", func(acc string) {
		chunks = append(chunks, acc)
	}, nil)
	if len(chunks) == 0 {
		t.Fatal("expected at least one streaming callback")
	}
	if chunks[len(chunks)-1] != "streamed" {
		t.Fatalf("final chunk want %q, got %q", "streamed", chunks[len(chunks)-1])
	}
}

func TestQuery_SendMessageFailure_ReturnError(t *testing.T) {
	// Server returns 500 for message POST — Query should propagate the error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(createSessionResponse{ID: "ses_fail"})
		case r.Method == http.MethodGet && r.URL.Path == "/global/event":
			// Keep SSE open until client cancels.
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			// All message POSTs fail.
			http.Error(w, "server error", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := newTestClient(srv.URL)
	_, err := c.Query(context.Background(), 42, "will fail", nil, nil)
	if err == nil {
		t.Fatal("expected error when message POST fails")
	}
}
