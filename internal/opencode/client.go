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

// Client communicates with the OpenCode HTTP server.
type Client struct {
	baseURL        string
	sessionTimeout time.Duration
	httpClient     *http.Client
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
		log:            log,
		sessions:       make(map[int64]string),
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

// sendMessageRequest is the JSON body for POST /session/{id}/message.
type sendMessageRequest struct {
	Parts []struct {
		Type    string `json:"type"`
		Content string `json:"content"`
		Text    string `json:"text"`
	} `json:"parts"`
}

// SendMessage posts a message to an OpenCode session.
func (c *Client) SendMessage(ctx context.Context, sessionID, content string) error {
	ctx, cancel := context.WithTimeout(ctx, c.sessionTimeout)
	defer cancel()

	url := c.baseURL + "/session/" + sessionID + "/message"
	body, err := json.Marshal(sendMessageRequest{
		Parts: []struct {
			Type    string `json:"type"`
			Content string `json:"content"`
			Text    string `json:"text"`
		}{
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

// StreamResponse connects to the SSE event stream for a session and calls
// onChunk with the accumulated response text as it arrives.
// Returns the final complete response text.
func (c *Client) StreamResponse(ctx context.Context, sessionID string, onChunk StreamCallback) (string, error) {
	url := c.baseURL + "/session/" + sessionID + "/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("stream request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	// Use a dedicated client for SSE streaming — no overall Timeout on the
	// http.Client (which would kill long-lived streams), but the context
	// carries a deadline so a stalled stream cannot hang forever.
	streamCtx, streamCancel := context.WithTimeout(ctx, c.sessionTimeout)
	defer streamCancel()
	req = req.WithContext(streamCtx)

	sseClient := &http.Client{}
	resp, err := sseClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("stream connect: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("stream: HTTP %d: %s", resp.StatusCode, string(body))
	}

	return c.readSSE(resp.Body, onChunk)
}

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
// OpenCode may use "content", "text", or "delta" depending on the response event type.
type contentDelta struct {
	Content string `json:"content"`
	Text    string `json:"text"`
	Delta   string `json:"delta"`
}

// readSSE reads an SSE stream and returns the accumulated response.
func (c *Client) readSSE(r io.Reader, onChunk StreamCallback) (string, error) {
	scanner := bufio.NewScanner(r)
	// SSE data lines can be large (e.g. full code blocks). Use 64KB initial / 1MB max
	// to avoid scanner buffer overflow on long assistant responses.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var accumulated strings.Builder
	var currentEvent sseEvent

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			// Empty line = end of event
			if len(currentEvent.Data) > 0 {
				text := c.extractText(currentEvent)
				if text != "" {
					accumulated.WriteString(text)
					if onChunk != nil {
						onChunk(accumulated.String())
					}
				}

				// Check for completion events
				if isCompletionEvent(currentEvent) {
					return accumulated.String(), nil
				}
			}
			currentEvent = sseEvent{}
			continue
		}

		if strings.HasPrefix(line, "event:") {
			currentEvent.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			// Per SSE spec, multiple data: lines are concatenated with newlines.
			// Preserve payload whitespace; only strip the single optional space
			// immediately following "data:" per the spec.
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimPrefix(data, " ")
			currentEvent.Data = append(currentEvent.Data, data)
		}
	}

	if err := scanner.Err(); err != nil {
		return accumulated.String(), fmt.Errorf("SSE read: %w", err)
	}

	// Stream ended (server closed connection)
	return accumulated.String(), nil
}

// extractText extracts displayable text from an SSE event.
func (c *Client) extractText(evt sseEvent) string {
	data := evt.dataString()
	if data == "" || data == "[DONE]" {
		return ""
	}

	var delta contentDelta
	if err := json.Unmarshal([]byte(data), &delta); err != nil {
		// Not JSON — return the raw data as plain text
		return data
	}

	// Try various field names used by different APIs
	if delta.Content != "" {
		return delta.Content
	}
	if delta.Text != "" {
		return delta.Text
	}
	if delta.Delta != "" {
		return delta.Delta
	}

	return ""
}

// isCompletionEvent returns true if the SSE event signals the end of a response.
func isCompletionEvent(evt sseEvent) bool {
	if evt.dataString() == "[DONE]" {
		return true
	}

	eventType := strings.ToLower(evt.Event)
	switch eventType {
	case "done", "complete", "message_stop", "message-complete", "finish":
		return true
	}

	return false
}

// Query sends a message to OpenCode and streams the response.
// It handles session creation/reuse for the given Telegram chat ID.
func (c *Client) Query(ctx context.Context, chatID int64, text string, onChunk StreamCallback) (string, error) {
	sessionID, err := c.GetOrCreateSession(ctx, chatID)
	if err != nil {
		return "", fmt.Errorf("get session: %w", err)
	}

	c.log.Log("QUERY [session=%s, chat=%d]: %s", sessionID, chatID, text)

	if err := c.SendMessage(ctx, sessionID, text); err != nil {
		// Session might be expired; try creating a new one
		c.log.Log("SendMessage failed, creating new session: %v", err)
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

	response, err := c.StreamResponse(ctx, sessionID, onChunk)
	if err != nil {
		return "", fmt.Errorf("stream response: %w", err)
	}

	trimmed := strings.TrimSpace(response)
	if trimmed == "" {
		trimmed = "(no response from OpenCode)"
	}

	maxLog := 200
	logMsg := trimmed
	if len(logMsg) > maxLog {
		logMsg = logMsg[:maxLog] + "…"
	}
	c.log.Log("RESPONSE [session=%s]: %s", sessionID, logMsg)

	return trimmed, nil
}
