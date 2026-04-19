// Package opencode provides an HTTP+SSE client for the OpenCode server API.
package opencode

import (
	"bufio"
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

const (
	// sseInitialBufferSize is the starting capacity of the bufio.Scanner buffer
	// used when reading SSE streams. Chosen to handle typical event payloads
	// without reallocation.
	sseInitialBufferSize = 64 * 1024 // 64 KB

	// sseMaxBufferSize is the upper bound on the scanner buffer. Large LLM
	// responses (e.g. code blocks) can produce very long data: lines.
	sseMaxBufferSize = 1024 * 1024 // 1 MB

	// sseLogSnippetLen is the maximum number of bytes logged per SSE data value.
	sseLogSnippetLen = 120
)

// Client communicates with the OpenCode HTTP server.
type Client struct {
	baseURL        string
	sessionTimeout time.Duration
	httpClient     *http.Client  // used for short-lived POST requests (has Timeout)
	sseHTTPClient  *http.Client  // used for SSE GET requests (no Timeout)
	log            *logger.Logger

	mu       sync.Mutex
	sessions map[int64]string // Telegram chatID → OpenCode sessionID
}

// NewClient creates a new OpenCode HTTP client.
func NewClient(baseURL string, sessionTimeout time.Duration, log *logger.Logger) *Client {
	return &Client{
		baseURL:        baseURL,
		sessionTimeout: sessionTimeout,
		httpClient:     &http.Client{Timeout: sessionTimeout},
		sseHTTPClient:  &http.Client{}, // no timeout — stream lifetime is bounded by ctx
		log:            log,
		sessions:       make(map[int64]string),
	}
}

// Close clears the cached sessions. Call on application exit.
func (c *Client) Close() {
	c.mu.Lock()
	c.sessions = make(map[int64]string)
	c.mu.Unlock()
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

// GetOrCreateSession returns an existing session for the chat, or creates one.
func (c *Client) GetOrCreateSession(ctx context.Context, chatID int64) (string, error) {
	c.mu.Lock()
	sid, ok := c.sessions[chatID]
	c.mu.Unlock()

	if ok {
		return sid, nil
	}

	sid, err := c.CreateSession(ctx)
	if err != nil {
		return "", err
	}

	c.mu.Lock()
	c.sessions[chatID] = sid
	c.mu.Unlock()

	return sid, nil
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
	c.log.Log("POST message [session=%s] url=%s", sessionID, url)

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

	c.log.Log("POST message [session=%s] accepted (HTTP %d)", sessionID, resp.StatusCode)
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

// globalEvent is the top-level wrapper for every event on the /global/event SSE stream.
type globalEvent struct {
	Payload globalPayload `json:"payload"`
}

// globalPayload holds the discriminated event type and its raw properties.
type globalPayload struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

// partDeltaProperties holds the fields of a "message.part.delta" event.
// These events carry individual text chunks while the assistant is streaming.
type partDeltaProperties struct {
	SessionID string `json:"sessionID"`
	Field     string `json:"field"`
	Delta     string `json:"delta"`
}

// sessionEventProperties holds the sessionID present in most session.* and
// message.* global events.
type sessionEventProperties struct {
	SessionID string `json:"sessionID"`
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
	case "done", "complete", "message_stop", "message-complete", "finish", "session.idle":
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

// StreamResponse opens a connection to the global SSE event stream at
// /global/event and streams assistant text for the given session until the
// server signals completion (session.idle) or closes the connection.
func (c *Client) StreamResponse(ctx context.Context, sessionID string, onChunk StreamCallback) (string, error) {
	url := c.baseURL + "/global/event"
	c.log.Log("SSE[%s] connecting to %s", sessionID, url)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("stream request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	// Use the dedicated SSE client which has no Timeout: the stream must stay
	// open for as long as OpenCode needs to generate the response. The caller's
	// ctx provides the only deadline (cancelled on bot shutdown).
	resp, err := c.sseHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("stream connect: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("stream: HTTP %d: %s", resp.StatusCode, string(body))
	}

	c.log.Log("SSE[%s] stream connected (HTTP 200)", sessionID)
	return c.readSSE(resp.Body, sessionID, onChunk)
}

// readSSE reads an SSE stream and returns the accumulated response text.
//
// For the /global/event stream, each data payload is a JSON-encoded globalEvent.
// Events for other sessions are silently skipped. Text is accumulated from
// "message.part.delta" events (field=="text"), and completion is signalled by
// a "session.idle" event for the current session.
//
// Termination rules:
//  1. A completion event is received → return immediately (success).
//  2. The server closes the stream (EOF) → success; if content was accumulated
//     it is returned as-is, if not the caller will show a fallback message.
//  3. Scanner error → return whatever was collected plus the error.
func (c *Client) readSSE(r io.Reader, sessionID string, onChunk StreamCallback) (string, error) {
	scanner := bufio.NewScanner(r)
	// Use the pre-defined buffer constants to handle large SSE payloads (e.g.
	// full code blocks) without scanner buffer overflow.
	scanner.Buffer(make([]byte, 0, sseInitialBufferSize), sseMaxBufferSize)

	var accumulated strings.Builder
	var currentEvent sseEvent
	eventCount := 0

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			// Empty line = end of one SSE event.
			if len(currentEvent.Data) > 0 {
				eventCount++
				data := currentEvent.dataString()

				// Try to parse as a /global/event envelope first.
				var ge globalEvent
				if json.Unmarshal([]byte(data), &ge) == nil && ge.Payload.Type != "" {
					c.log.Log("SSE[%s] event #%d type=%q data=%s", sessionID, eventCount,
						ge.Payload.Type, sseDataSnippet(data))

					// Extract the sessionID from the event properties (present on
					// most session.* and message.* events). Skip events that belong
					// to a different session. Unmarshal errors are intentionally
					// ignored: events like server.connected/server.heartbeat carry no
					// sessionID and props.SessionID will simply be "".
					var props sessionEventProperties
					_ = json.Unmarshal(ge.Payload.Properties, &props)
					if props.SessionID != "" && props.SessionID != sessionID {
						currentEvent = sseEvent{}
						continue
					}

					// Extract streaming text from message.part.delta events.
					if ge.Payload.Type == "message.part.delta" {
						var delta partDeltaProperties
						deltaErr := json.Unmarshal(ge.Payload.Properties, &delta)
						if deltaErr == nil && delta.Field == "text" && delta.Delta != "" {
							accumulated.WriteString(delta.Delta)
							if onChunk != nil {
								onChunk(accumulated.String())
							}
						}
					}

					// session.idle signals that the assistant has finished responding.
					if ge.Payload.Type == "session.idle" {
						c.log.Log("SSE[%s] completion event detected after %d events", sessionID, eventCount)
						return accumulated.String(), nil
					}

					currentEvent = sseEvent{}
					continue
				}

				// Fallback: handle non-global-format events using the legacy logic.
				// Decode JSON once per event; share the result between extractText
				// and isCompletionEvent to avoid redundant work.
				var delta contentDelta
				deltaOK := data != "" && data != "[DONE]" && json.Unmarshal([]byte(data), &delta) == nil

				c.log.Log("SSE[%s] event #%d type=%q data=%s", sessionID, eventCount,
					currentEvent.Event, sseDataSnippet(data))

				text := extractText(data, deltaOK, &delta)
				if text != "" {
					accumulated.WriteString(text)
					if onChunk != nil {
						onChunk(accumulated.String())
					}
				}

				if isCompletionEvent(currentEvent.Event, data, deltaOK, &delta) {
					c.log.Log("SSE[%s] completion event detected after %d events", sessionID, eventCount)
					return accumulated.String(), nil
				}
			}
			currentEvent = sseEvent{}
			continue
		}

		if strings.HasPrefix(line, "event:") {
			currentEvent.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			// Per SSE spec: strip the single optional space immediately after "data:".
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimPrefix(data, " ")
			currentEvent.Data = append(currentEvent.Data, data)
		}
	}

	if err := scanner.Err(); err != nil {
		return accumulated.String(), fmt.Errorf("SSE read error: %w", err)
	}

	// Server closed the connection (EOF). This is the normal end-of-response
	// signal for OpenCode: it closes /events once the assistant reply is complete.
	acc := accumulated.String()
	if acc != "" {
		c.log.Log("SSE[%s] stream closed by server after %d events — response complete", sessionID, eventCount)
	} else {
		c.log.Log("SSE[%s] stream closed by server with no text content (%d events processed)", sessionID, eventCount)
	}
	// Return nil error regardless: EOF is a normal, successful termination.
	return acc, nil
}

// sseDataSnippet returns a short excerpt of a raw SSE data string for logging.
func sseDataSnippet(data string) string {
	if len(data) <= sseLogSnippetLen {
		return data
	}
	return data[:sseLogSnippetLen] + "…"
}

// Query sends a message to OpenCode and streams the response back.
// It handles session creation/reuse for the given Telegram chat ID.
func (c *Client) Query(ctx context.Context, chatID int64, text string, onChunk StreamCallback) (string, error) {
	sessionID, err := c.GetOrCreateSession(ctx, chatID)
	if err != nil {
		return "", fmt.Errorf("get session: %w", err)
	}

	c.log.Log("QUERY [session=%s, chat=%d]: %s", sessionID, chatID, text)

	if err := c.SendMessage(ctx, sessionID, text); err != nil {
		// Session might have expired; try creating a new one.
		c.log.Log("SendMessage failed [session=%s], creating new session: %v", sessionID, err)
		c.mu.Lock()
		delete(c.sessions, chatID)
		c.mu.Unlock()

		sessionID, err = c.GetOrCreateSession(ctx, chatID)
		if err != nil {
			return "", fmt.Errorf("recreate session: %w", err)
		}

		if err := c.SendMessage(ctx, sessionID, text); err != nil {
			return "", fmt.Errorf("send message (retry): %w", err)
		}
	}

	c.log.Log("SSE[%s] opening event stream for query", sessionID)
	response, err := c.StreamResponse(ctx, sessionID, onChunk)
	if err != nil {
		return "", fmt.Errorf("stream response: %w", err)
	}

	trimmed := strings.TrimSpace(response)
	if trimmed == "" {
		trimmed = "(no response from OpenCode)"
	}

	logMsg := trimmed
	if len(logMsg) > 200 {
		logMsg = logMsg[:200] + "…"
	}
	c.log.Log("RESPONSE [session=%s]: %s", sessionID, logMsg)

	return trimmed, nil
}
