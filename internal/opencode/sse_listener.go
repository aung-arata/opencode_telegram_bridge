package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	sseReconnectBaseDelay = 1 * time.Second
	sseReconnectMaxDelay  = 30 * time.Second
)

// errSessionGone is returned by connect when the server reports (HTTP 404) that the session no longer exists.
var errSessionGone = errors.New("server reported session not found")

// pendingQuery holds the state for an in-flight query waiting for an SSE response.
type pendingQuery struct {
	onChunk  StreamCallback
	accum    strings.Builder
	resultCh chan queryResult // receives exactly one value when the response completes or fails
}

// queryResult carries the outcome of a completed response.
type queryResult struct {
	text string
	err  error
}

// sseListener maintains a persistent SSE connection to /session/{id}/events for one
// OpenCode session. A single dedicated goroutine owns the HTTP connection; callers
// register a pendingQuery to receive the next assistant response.
type sseListener struct {
	sessionID string
	client    *Client

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	pending *pendingQuery
}

// newSSEListener creates a listener and immediately starts the background goroutine.
func newSSEListener(ctx context.Context, sessionID string, client *Client) *sseListener {
	lctx, cancel := context.WithCancel(ctx)
	l := &sseListener{
		sessionID: sessionID,
		client:    client,
		ctx:       lctx,
		cancel:    cancel,
	}
	go l.run()
	return l
}

// arm registers pq to receive the next assistant response.
func (l *sseListener) arm(pq *pendingQuery) {
	l.mu.Lock()
	l.pending = pq
	l.mu.Unlock()
}

// disarm clears the pending slot without sending a result.
// Used when SendMessage fails before any SSE events for the current query arrive.
func (l *sseListener) disarm() {
	l.mu.Lock()
	l.pending = nil
	l.mu.Unlock()
}

// failPending delivers err to any waiting query and clears the pending slot.
func (l *sseListener) failPending(err error) {
	l.mu.Lock()
	pq := l.pending
	l.pending = nil
	l.mu.Unlock()
	if pq == nil {
		return
	}
	select {
	case pq.resultCh <- queryResult{err: err}:
	default:
	}
}

// close shuts the listener down, cancelling any in-flight query.
func (l *sseListener) close() {
	l.cancel()
}

// run is the persistent reconnect loop. It exits only when the context is cancelled
// or the server reports the session is gone.
func (l *sseListener) run() {
	delay := sseReconnectBaseDelay
	for {
		start := time.Now()
		err := l.connect()

		if l.ctx.Err() != nil {
			// Clean shutdown triggered by close() or Client.Close().
			l.failPending(fmt.Errorf("session closed"))
			return
		}

		if errors.Is(err, errSessionGone) {
			l.client.log.Log("SSE[%s] session gone — stopping listener", l.sessionID)
			l.failPending(fmt.Errorf("OpenCode session expired; send a new message to start a fresh session"))
			l.client.removeListener(l)
			return
		}

		// Reset backoff if the connection was stable for a reasonable duration.
		if time.Since(start) > 10*time.Second {
			delay = sseReconnectBaseDelay
		}

		if err != nil {
			l.client.log.Log("SSE[%s] stream error: %v — reconnecting in %s", l.sessionID, err, delay)
		} else {
			l.client.log.Log("SSE[%s] stream closed — reconnecting in %s", l.sessionID, delay)
		}

		// Fail any pending query so it does not wait forever while we reconnect.
		l.failPending(fmt.Errorf("SSE stream interrupted"))

		select {
		case <-l.ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, sseReconnectMaxDelay)
	}
}

// connect opens the SSE HTTP stream and reads events until EOF, error, or context
// cancellation. Returns nil on context cancellation, errSessionGone on HTTP 404,
// or an I/O / HTTP error.
func (l *sseListener) connect() error {
	url := l.client.baseURL + "/session/" + l.sessionID + "/events"
	req, err := http.NewRequestWithContext(l.ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		if l.ctx.Err() != nil {
			return nil // context cancelled, not a real transport error
		}
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// fall through to read events
	case http.StatusNotFound:
		return errSessionGone
	default:
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return l.readEvents(resp.Body)
}

// readEvents consumes the SSE stream until EOF, scanner error, or context cancellation.
func (l *sseListener) readEvents(r io.Reader) error {
	scanner := bufio.NewScanner(r)
	// Use a large buffer to handle big assistant payloads without overflow.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var current sseEvent
	for scanner.Scan() {
		if l.ctx.Err() != nil {
			return nil
		}
		line := scanner.Text()

		if line == "" {
			// Blank line signals end of one SSE event.
			if len(current.Data) > 0 {
				l.processEvent(current)
			}
			current = sseEvent{}
			continue
		}

		if strings.HasPrefix(line, "event:") {
			current.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			// Per the SSE spec: strip the single optional space after "data:".
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimPrefix(data, " ")
			current.Data = append(current.Data, data)
		}
	}
	return scanner.Err()
}

// processEvent extracts text and completion signals from one SSE event and forwards
// them to the active pendingQuery (if any).
func (l *sseListener) processEvent(evt sseEvent) {
	data := evt.dataString()
	var delta contentDelta
	deltaOK := data != "" && data != "[DONE]" && json.Unmarshal([]byte(data), &delta) == nil

	text := extractText(data, deltaOK, &delta)
	done := isCompletionEvent(evt.Event, data, deltaOK, &delta)

	// Snapshot and (on completion) clear the pending query while holding the
	// lock, then release before invoking callbacks to avoid holding the mutex
	// during potentially slow Telegram API calls.
	l.mu.Lock()
	pq := l.pending
	if done && pq != nil {
		l.pending = nil
	}
	l.mu.Unlock()

	if pq == nil {
		return
	}

	if text != "" {
		pq.accum.WriteString(text)
		if pq.onChunk != nil {
			pq.onChunk(pq.accum.String())
		}
	}

	if done {
		result := strings.TrimSpace(pq.accum.String())
		if result == "" {
			result = "(no response from OpenCode)"
		}
		select {
		case pq.resultCh <- queryResult{text: result}:
		default:
		}
	}
}
