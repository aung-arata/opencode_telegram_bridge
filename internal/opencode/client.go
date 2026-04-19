// Package opencode provides an HTTP+SSE client for the OpenCode server API.
package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aung-arata/opencode-telegram-bridge/internal/logger"
)

// Client communicates with the OpenCode HTTP server.
type Client struct {
	baseURL        string
	sessionTimeout time.Duration
	httpClient     *http.Client
	log            *logger.Logger

	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	listeners map[int64]*sseListener // Telegram chatID → persistent SSE listener
}

// NewClient creates a new OpenCode HTTP client.
func NewClient(baseURL string, sessionTimeout time.Duration, log *logger.Logger) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{
		baseURL:        baseURL,
		sessionTimeout: sessionTimeout,
		httpClient:     &http.Client{Timeout: sessionTimeout},
		log:            log,
		ctx:            ctx,
		cancel:         cancel,
		listeners:      make(map[int64]*sseListener),
	}
}

// Close shuts down all persistent SSE connections and releases associated resources.
// It should be called when the application exits.
func (c *Client) Close() {
	c.cancel()
	c.mu.Lock()
	listeners := c.listeners
	c.listeners = make(map[int64]*sseListener)
	c.mu.Unlock()
	for _, l := range listeners {
		l.close()
	}
}

// createSessionResponse is the JSON returned by POST /session.
type createSessionResponse struct {
	ID string `json:"id"`
}

// CreateSession creates a new OpenCode session and returns its ID.
func (c *Client) CreateSession(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.sessionTimeout)
	defer cancel()

	url := c.baseURL + "/session"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", fmt.Errorf("create session request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("create session: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result createSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode session response: %w", err)
	}

	if result.ID == "" {
		return "", fmt.Errorf("create session: empty session ID in response")
	}

	c.log.Log("OpenCode session created: %s", result.ID)
	return result.ID, nil
}

// getOrStartListener returns (or lazily creates) the persistent SSE listener for the
// given Telegram chat ID. On first call for a chat ID it creates an OpenCode session
// and starts the background SSE goroutine.
func (c *Client) getOrStartListener(ctx context.Context, chatID int64) (*sseListener, error) {
	c.mu.Lock()
	l, ok := c.listeners[chatID]
	c.mu.Unlock()
	if ok {
		return l, nil
	}

	sessionID, err := c.CreateSession(ctx)
	if err != nil {
		return nil, err
	}

	// The listener uses c.ctx so it outlives any individual query context and is
	// only stopped by Client.Close() or a server-side session-gone response.
	l = newSSEListener(c.ctx, sessionID, c)
	c.mu.Lock()
	c.listeners[chatID] = l
	c.mu.Unlock()
	return l, nil
}

// removeListener removes l from the listener map (called by the SSE goroutine
// when the server reports the session is gone).
func (c *Client) removeListener(l *sseListener) {
	c.mu.Lock()
	for chatID, listener := range c.listeners {
		if listener == l {
			delete(c.listeners, chatID)
			break
		}
	}
	c.mu.Unlock()
}

// contentPart represents a single entry in a "parts" array, used in both
// outgoing message requests and incoming SSE event payloads.
// Each part has a "type" (e.g. "text", "step-start", "step-finish") and an
// optional "text" / "content" field carrying the actual message text.
type contentPart struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Content string `json:"content"`
}

// sendMessageRequest is the JSON body for POST /session/{id}/message.
type sendMessageRequest struct {
	Parts []contentPart `json:"parts"`
}

// SendMessage posts a message to an OpenCode session.
func (c *Client) SendMessage(ctx context.Context, sessionID, content string) error {
	ctx, cancel := context.WithTimeout(ctx, c.sessionTimeout)
	defer cancel()

	url := c.baseURL + "/session/" + sessionID + "/message"
	body, err := json.Marshal(sendMessageRequest{
		Parts: []contentPart{
			{Type: "text", Content: content, Text: content},
		},
	})
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("send message request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send message: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("send message: HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

// StreamCallback is called with accumulated text chunks during SSE streaming.
type StreamCallback func(accumulated string)

// sseEvent represents a parsed Server-Sent Event.
type sseEvent struct {
	Event string
	Data  []string // multiple data: lines are collected per SSE spec
}

// dataString returns the event data with multiple lines joined by newlines (per SSE spec).
func (e sseEvent) dataString() string {
	return strings.Join(e.Data, "\n")
}

// contentDelta represents JSON fields that may carry text content in SSE data.
// OpenCode may use "content", "text", "delta", or a nested "parts" array
// depending on the response event type.
type contentDelta struct {
	Content string        `json:"content"`
	Text    string        `json:"text"`
	Delta   string        `json:"delta"`
	Parts   []contentPart `json:"parts"`
}

// extractText extracts displayable text from a pre-parsed SSE event payload.
// data is the raw data string; deltaOK reports whether delta was successfully
// unmarshaled from it.
func extractText(data string, deltaOK bool, delta *contentDelta) string {
	if data == "" || data == "[DONE]" {
		return ""
	}

	if !deltaOK {
		// Not JSON — return the raw data as plain text.
		return data
	}

	// Try top-level field names used by different APIs.
	if delta.Content != "" {
		return delta.Content
	}
	if delta.Text != "" {
		return delta.Text
	}
	if delta.Delta != "" {
		return delta.Delta
	}

	// OpenCode wraps response text inside a "parts" array.
	// Concatenate all parts whose type is "text".
	var sb strings.Builder
	for _, part := range delta.Parts {
		if part.Type != "text" {
			continue
		}
		if part.Text != "" {
			sb.WriteString(part.Text)
		} else if part.Content != "" {
			sb.WriteString(part.Content)
		}
	}
	return sb.String()
}

// isCompletionEvent returns true if the SSE event signals the end of a response.
// eventName is the "event:" field value; data is the raw payload; deltaOK and
// delta come from a single JSON decode performed by the caller.
func isCompletionEvent(eventName, data string, deltaOK bool, delta *contentDelta) bool {
	if data == "[DONE]" {
		return true
	}

	switch strings.ToLower(eventName) {
	case "done", "complete", "message_stop", "message-complete", "finish":
		return true
	}

	// OpenCode signals completion via a part with type "step-finish" or "message-finish".
	if deltaOK {
		for _, part := range delta.Parts {
			t := strings.ToLower(part.Type)
			if t == "step-finish" || t == "message-finish" || t == "finish" {
				return true
			}
		}
	}

	return false
}

// Query sends a message to OpenCode and streams the response via the session's
// persistent SSE connection. It handles session creation/reuse for the given
// Telegram chat ID.
func (c *Client) Query(ctx context.Context, chatID int64, text string, onChunk StreamCallback) (string, error) {
	listener, err := c.getOrStartListener(ctx, chatID)
	if err != nil {
		return "", fmt.Errorf("get session: %w", err)
	}

	c.log.Log("QUERY [session=%s, chat=%d]: %s", listener.sessionID, chatID, text)

	pq := &pendingQuery{
		onChunk:  onChunk,
		resultCh: make(chan queryResult, 1),
	}
	listener.arm(pq)

	if err := c.SendMessage(ctx, listener.sessionID, text); err != nil {
		listener.disarm()

		// Session might have expired; remove the stale listener and create a new one.
		c.log.Log("SendMessage failed, recreating session: %v", err)
		c.mu.Lock()
		delete(c.listeners, chatID)
		c.mu.Unlock()
		listener.close()

		listener, err = c.getOrStartListener(ctx, chatID)
		if err != nil {
			return "", fmt.Errorf("recreate session: %w", err)
		}

		pq = &pendingQuery{
			onChunk:  onChunk,
			resultCh: make(chan queryResult, 1),
		}
		listener.arm(pq)

		if err := c.SendMessage(ctx, listener.sessionID, text); err != nil {
			listener.disarm()
			return "", fmt.Errorf("send message (retry): %w", err)
		}
	}

	select {
	case result := <-pq.resultCh:
		if result.err != nil {
			return "", fmt.Errorf("stream response: %w", result.err)
		}
		logMsg := result.text
		if len(logMsg) > 200 {
			logMsg = logMsg[:200] + "…"
		}
		c.log.Log("RESPONSE [session=%s]: %s", listener.sessionID, logMsg)
		return result.text, nil
	case <-ctx.Done():
		listener.disarm()
		return "", ctx.Err()
	}
}
