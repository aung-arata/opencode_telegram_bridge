// Package telegram implements the Telegram polling bot with command dispatch.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/aung-arata/opencode-telegram-bridge/internal/config"
	"github.com/aung-arata/opencode-telegram-bridge/internal/logger"
	"github.com/aung-arata/opencode-telegram-bridge/internal/opencode"
)

const (
	// telegramMaxMessageLen is the maximum character count Telegram allows in a single message.
	telegramMaxMessageLen = 4096
	// streamingTruncateLen is the rune limit for in-progress streaming edits (leaves room for suffix).
	streamingTruncateLen = 4000
	// minEditInterval throttles message edits to stay within Telegram's ~1 edit/second/chat rate limit.
	minEditInterval = 1 * time.Second
	// permCallbackPrefix is the prefix for inline keyboard callback data for permissions.
	permCallbackPrefix = "perm:"
)

// pendingPerm tracks a permission request that is waiting for user approval.
type pendingPerm struct {
	chatID    int64
	msgID     int
	sessionID string
}

// Bot is the Telegram polling bot.
type Bot struct {
	api *tgbotapi.BotAPI
	cfg *config.Config
	log *logger.Logger
	oc  *opencode.Client
	mu  sync.Mutex // serializes OpenCode queries

	permMu       sync.Mutex
	pendingPerms map[string]pendingPerm // permissionID → pendingPerm
}

// NewBot creates a new Telegram bot instance.
func NewBot(cfg *config.Config, log *logger.Logger, oc *opencode.Client) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(cfg.TGBotToken)
	if err != nil {
		return nil, fmt.Errorf("telegram bot init: %w", err)
	}

	log.Log("Telegram bot authorized as @%s", api.Self.UserName)

	return &Bot{
		api:          api,
		cfg:          cfg,
		log:          log,
		oc:           oc,
		pendingPerms: make(map[string]pendingPerm),
	}, nil
}

// Run starts the polling loop. It blocks until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) {
	lastUpdateID := b.loadLastUpdateID()
	b.log.Log("Bot started. Waiting for commands... (last_update_id=%d)", lastUpdateID)

	u := tgbotapi.NewUpdate(int(lastUpdateID + 1))
	u.Timeout = 30

	updates := b.api.GetUpdatesChan(u)

	for {
		select {
		case <-ctx.Done():
			b.log.Log("Shutting down bot polling...")
			b.api.StopReceivingUpdates()
			return
		case update, ok := <-updates:
			if !ok {
				return
			}
			switch {
			case update.Message != nil:
				go func(msg *tgbotapi.Message) {
					b.handleMessage(ctx, msg)
				}(update.Message)
			case update.CallbackQuery != nil:
				b.handleCallbackQuery(ctx, update.CallbackQuery)
			}
			b.saveLastUpdateID(int64(update.UpdateID))
		}
	}
}

// handleMessage processes a single incoming message.
func (b *Bot) handleMessage(ctx context.Context, msg *tgbotapi.Message) {
	if msg.From == nil {
		return
	}

	senderID := msg.From.ID
	chatID := msg.Chat.ID
	text := msg.Text
	msgID := msg.MessageID

	if senderID != b.cfg.TGUserID {
		return // silently ignore unauthorized senders
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	switch {
	case text == "/help":
		b.cmdHelp(chatID, msgID)
	case text == "/start":
		b.cmdHelp(chatID, msgID)
	case text == "/newsession":
		b.cmdNewSession(chatID, msgID)
	case text == "/abort":
		b.cmdAbort(ctx, chatID, msgID)
	case text == "/undo":
		b.cmdUndo(ctx, chatID, msgID)
	case text == "/sessions":
		b.cmdSessions(ctx, chatID, msgID)
	case text == "/diff":
		b.cmdDiff(ctx, chatID, msgID)
	case text == "/plan":
		b.cmdSwitchMode(ctx, chatID, msgID, "plan")
	case text == "/build":
		b.cmdSwitchMode(ctx, chatID, msgID, "")
	case strings.HasPrefix(text, "/echo "):
		b.sendReply(chatID, msgID, strings.TrimPrefix(text, "/echo "))
	case strings.HasPrefix(text, "/ask "):
		query := strings.TrimSpace(strings.TrimPrefix(text, "/ask "))
		if query != "" {
			b.handleQuery(ctx, chatID, msgID, query)
		}
	case !strings.HasPrefix(text, "/"):
		b.handleQuery(ctx, chatID, msgID, text)
	}
}

// cmdHelp sends the help message.
func (b *Bot) cmdHelp(chatID int64, replyTo int) {
	help := "🤖 OpenCode Telegram Bridge\n\n" +
		"Send any plain message → forwarded to OpenCode as a query.\n\n" +
		"Commands:\n" +
		"/ask <query>  — explicit OpenCode query\n" +
		"/abort        — cancel the running query\n" +
		"/undo         — revert the last message and its file changes\n" +
		"/sessions     — list all sessions with titles\n" +
		"/diff         — show files changed in current session\n" +
		"/plan         — switch to plan mode (read-only analysis)\n" +
		"/build        — switch to build mode (full access, default)\n" +
		"/newsession   — start a fresh OpenCode session\n" +
		"/echo <msg>   — bot replies with your message\n" +
		"/help         — show this help\n\n" +
		fmt.Sprintf("OpenCode URL: %s\n", b.cfg.OpenCodeURL) +
		fmt.Sprintf("Session timeout: %s\n", b.cfg.OpenCodeSessionTimeout) +
		fmt.Sprintf("Log: %s", b.cfg.LogFile)
	b.sendReply(chatID, replyTo, help)
}

// cmdAbort cancels any active OpenCode query for the chat by calling the
// abort endpoint. The running SSE stream will receive a session.idle event
// and terminate naturally, editing the placeholder with whatever was streamed
// up to the abort.
func (b *Bot) cmdAbort(ctx context.Context, chatID int64, replyTo int) {
	sid, ok := b.oc.GetSession(chatID)
	if !ok {
		b.sendReply(chatID, replyTo, "⚠️ No active session to abort.")
		return
	}
	if err := b.oc.Abort(ctx, sid); err != nil {
		b.log.Log("Abort error [chat=%d session=%s]: %v", chatID, sid, err)
		b.sendReply(chatID, replyTo, fmt.Sprintf("❌ Abort failed: %v", err))
		return
	}
	b.sendReply(chatID, replyTo, "⏹ Query aborted.")
}

// cmdDiff shows files changed in the current session via GET /session/:id/diff.
// Does not acquire b.mu — read-only, and a mid-flight /newsession returning
// stale diff data is acceptable for a diagnostic command.
// sendReply uses plain text (no ParseMode), so filenames need no escaping.
func (b *Bot) cmdDiff(ctx context.Context, chatID int64, replyTo int) {
	sid, ok := b.oc.GetSession(chatID)
	if !ok {
		b.sendReply(chatID, replyTo, "⚠️ No active session.")
		return
	}
	diffs, err := b.oc.Diff(ctx, sid)
	if err != nil {
		b.log.Log("Diff error [chat=%d session=%s]: %v", chatID, sid, err)
		b.sendReply(chatID, replyTo, fmt.Sprintf("❌ Diff failed: %v", err))
		return
	}
	if len(diffs) == 0 {
		b.sendReply(chatID, replyTo, "✅ No file changes in current session.")
		return
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📝 %d file(s) changed:\n\n", len(diffs)))
	for _, d := range diffs {
		sb.WriteString(fmt.Sprintf("`%s` +%d -%d\n", d.File, d.Additions, d.Deletions))
	}
	b.sendReply(chatID, replyTo, sb.String())
}

// cmdUndo reverts the last user message and its file changes via
// GET /session/:id/message (to find the last user messageID) then
// POST /session/:id/revert.
// Acquires b.mu so it cannot race with an in-progress query or session switch.
func (b *Bot) cmdUndo(ctx context.Context, chatID int64, replyTo int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	sid, ok := b.oc.GetSession(chatID)
	if !ok {
		b.sendReply(chatID, replyTo, "⚠️ No active session.")
		return
	}
	msgs, err := b.oc.Messages(ctx, sid)
	if err != nil {
		b.log.Log("Undo messages error [chat=%d session=%s]: %v", chatID, sid, err)
		b.sendReply(chatID, replyTo, fmt.Sprintf("❌ Undo failed: %v", err))
		return
	}
	lastID := lastUserMessageID(msgs)
	if lastID == "" {
		b.sendReply(chatID, replyTo, "⚠️ No messages to undo.")
		return
	}
	if err := b.oc.Revert(ctx, sid, lastID); err != nil {
		b.log.Log("Undo revert error [chat=%d session=%s]: %v", chatID, sid, err)
		b.sendReply(chatID, replyTo, fmt.Sprintf("❌ Undo failed: %v", err))
		return
	}
	b.sendReply(chatID, replyTo, "↩️ Last message reverted.")
}

// cmdSessions lists all sessions known to the OpenCode server with their titles.
func (b *Bot) cmdSessions(ctx context.Context, chatID int64, replyTo int) {
	sessions, err := b.oc.ListSessions(ctx)
	if err != nil {
		b.log.Log("Sessions error [chat=%d]: %v", chatID, err)
		b.sendReply(chatID, replyTo, fmt.Sprintf("❌ Sessions failed: %v", err))
		return
	}
	if len(sessions) == 0 {
		b.sendReply(chatID, replyTo, "No sessions found.")
		return
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📋 %d session(s):\n\n", len(sessions)))
	for _, s := range sessions {
		title := s.Title
		if title == "" {
			title = "(untitled)"
		}
		sb.WriteString(fmt.Sprintf("`%s` %s\n", s.ID, title))
	}
	b.sendReply(chatID, replyTo, sb.String())
}

// lastUserMessageID returns the ID of the last message with role "user", or ""
// if none exists. Used by cmdUndo to find what to revert.
func lastUserMessageID(msgs []opencode.MessageInfo) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].ID
		}
	}
	return ""
}

// cmdNewSession drops the current OpenCode session so the next query starts fresh.
// It acquires b.mu so that any in-progress query finishes before the session is
// cleared, preventing two concurrent sessions for the same chat.
func (b *Bot) cmdNewSession(chatID int64, replyTo int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.oc.ResetSession(chatID)
	b.log.Log("Session reset for chat=%d", chatID)
	b.sendReply(chatID, replyTo, "\U0001F195 Session context cleared. Your next message will start a fresh OpenCode session.")
}

// cmdSwitchMode creates a new OpenCode session with the given agent mode and
// makes it the active session for the chat.
// agent="" → build (default, full file access)
// agent="plan" → plan (read-only analysis, no file writes)
func (b *Bot) cmdSwitchMode(ctx context.Context, chatID int64, replyTo int, agent string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	sid, err := b.oc.SwitchMode(ctx, chatID, agent)
	if err != nil {
		b.log.Log("SwitchMode error [chat=%d agent=%q]: %v", chatID, agent, err)
		b.sendReply(chatID, replyTo, fmt.Sprintf("❌ Failed to switch mode: %v", err))
		return
	}

	var reply string
	if agent == "plan" {
		reply = "📋 Switched to plan mode — read-only analysis, no file changes.\n\nSession: " + sid
	} else {
		reply = "🔨 Switched to build mode — full access.\n\nSession: " + sid
	}
	b.log.Log("Mode switch [chat=%d agent=%q session=%s]", chatID, agent, sid)
	b.sendReply(chatID, replyTo, reply)
}

// handleQuery sends a query to OpenCode and streams the response back.
func (b *Bot) handleQuery(ctx context.Context, chatID int64, replyTo int, query string) {
	// Send a placeholder message that we'll edit with streaming content
	placeholder := b.sendReply(chatID, replyTo, "⏳ Thinking...")
	if placeholder == nil {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	var lastEdit time.Time

	onChunk := func(accumulated string) {
		if time.Since(lastEdit) < minEditInterval {
			return
		}
		text := accumulated
		if utf8.RuneCountInString(text) > streamingTruncateLen {
			text = truncateRunes(text, streamingTruncateLen) + "\n\n... (truncated, streaming)"
		}
		edit := tgbotapi.NewEditMessageText(chatID, placeholder.MessageID, text)
		if _, err := b.api.Send(edit); err != nil {
			// Ignore "message is not modified" errors
			if !strings.Contains(err.Error(), "message is not modified") {
				b.log.Log("Edit message error: %v", err)
			}
		}
		lastEdit = time.Now()
	}

	response, err := b.oc.Query(ctx, chatID, query, onChunk, func(req opencode.PermissionRequest) {
		if isDangerous(req) {
			b.handlePermissionRequest(chatID, req)
		} else {
			b.autoApprovePermission(ctx, chatID, req)
		}
	})
	if err != nil {
		b.log.Log("OpenCode query error: %v", err)
		var editText string
		if isTimeoutError(err) {
			editText = "⏱ Query timed out — OpenCode is still processing.\n\nSend /abort to cancel, or wait and retry."
		} else {
			editText = fmt.Sprintf("❌ OpenCode error: %v", err)
		}
		edit := tgbotapi.NewEditMessageText(chatID, placeholder.MessageID, editText)
		b.api.Send(edit)
		return
	}

	// Send the final response by editing the placeholder
	if utf8.RuneCountInString(response) > telegramMaxMessageLen {
		// Split into multiple messages at rune boundaries
		chunks := splitRunes(response, telegramMaxMessageLen)
		edit := tgbotapi.NewEditMessageText(chatID, placeholder.MessageID, chunks[0])
		b.api.Send(edit)
		for _, chunk := range chunks[1:] {
			b.sendReply(chatID, replyTo, chunk)
		}
	} else {
		edit := tgbotapi.NewEditMessageText(chatID, placeholder.MessageID, response)
		if _, err := b.api.Send(edit); err != nil {
			if !strings.Contains(err.Error(), "message is not modified") {
				b.log.Log("Final edit error: %v", err)
			}
		}
	}
}

// dangerousCommands is the set of bash commands that require explicit user approval.
var dangerousCommands = []string{
	"rm", "rmdir", "dd", "shred", "truncate", "mkfs", "wipefs", "parted", "fdisk",
}

// isDangerous reports whether a permission request should be presented to the
// user for approval. Safe read/list/create operations are auto-approved.
func isDangerous(req opencode.PermissionRequest) bool {
	switch req.Permission {
	case "external_directory":
		// Any out-of-project directory access requires explicit approval.
		return true
	case "bash":
		for _, p := range req.Patterns {
			if containsDangerousCommand(p) {
				return true
			}
		}
		return false
	default:
		// Unknown tools: auto-approve (read_file, list_directory, etc.).
		return false
	}
}

// containsDangerousCommand returns true if the bash pattern invokes a
// destructive command either as the first token or inside a pipeline/chain.
func containsDangerousCommand(pattern string) bool {
	pattern = strings.TrimSpace(pattern)
	parts := strings.Fields(pattern)
	if len(parts) == 0 {
		return false
	}
	// Resolve the base command name (handles /bin/rm → rm, /usr/bin/dd → dd).
	first := strings.ToLower(filepath.Base(parts[0]))
	for _, d := range dangerousCommands {
		// Exact match or variant like mkfs.ext4 matching mkfs.
		if first == d || strings.HasPrefix(first, d+".") {
			return true
		}
	}
	// Check for dangerous commands embedded in a pipeline or command chain.
	lower := strings.ToLower(pattern)
	for _, d := range dangerousCommands {
		if strings.Contains(lower, " "+d+" ") ||
			strings.Contains(lower, "|"+d+" ") ||
			strings.Contains(lower, ";"+d+" ") ||
			strings.HasSuffix(lower, " "+d) {
			return true
		}
	}
	return false
}

// autoApprovePermission responds to a permission request with "always" without
// user interaction and sends a brief informational message to the chat.
func (b *Bot) autoApprovePermission(ctx context.Context, chatID int64, req opencode.PermissionRequest) {
	patterns := strings.Join(req.Patterns, ", ")
	if patterns == "" {
		patterns = "(any)"
	}
	if err := b.oc.RespondToPermission(ctx, req.SessionID, req.ID, "always"); err != nil {
		b.log.Log("Auto-approve failed [perm=%s tool=%s]: %v", req.ID, req.Permission, err)
		// Fall back to the manual keyboard.
		b.handlePermissionRequest(chatID, req)
		return
	}
	b.log.Log("Auto-approved [tool=%s patterns=%s]", req.Permission, patterns)
	b.sendReply(chatID, 0, fmt.Sprintf("✅ Auto-approved: %s (%s)", req.Permission, patterns))
}

// handlePermissionRequest sends a Telegram message with an inline keyboard
// asking the user to approve or reject an OpenCode permission request.
func (b *Bot) handlePermissionRequest(chatID int64, req opencode.PermissionRequest) {
	patterns := strings.Join(req.Patterns, ", ")
	if patterns == "" {
		patterns = "(any)"
	}
	text := fmt.Sprintf(
		"🔐 OpenCode needs permission\n\n"+
			"Tool: %s\n"+
			"Patterns: %s",
		req.Permission, patterns,
	)

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Once", permCallbackPrefix+req.ID+":once"),
			tgbotapi.NewInlineKeyboardButtonData("♾ Always", permCallbackPrefix+req.ID+":always"),
			tgbotapi.NewInlineKeyboardButtonData("❌ Reject", permCallbackPrefix+req.ID+":reject"),
		),
	)

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyMarkup = keyboard
	sent, err := b.api.Send(msg)
	if err != nil {
		b.log.Log("Failed to send permission request message: %v", err)
		return
	}

	b.permMu.Lock()
	b.pendingPerms[req.ID] = pendingPerm{
		chatID:    chatID,
		msgID:     sent.MessageID,
		sessionID: req.SessionID,
	}
	b.permMu.Unlock()
}

// handleCallbackQuery processes an inline keyboard button press.
func (b *Bot) handleCallbackQuery(ctx context.Context, cq *tgbotapi.CallbackQuery) {
	// Always acknowledge the callback to clear Telegram's loading spinner.
	defer func() {
		answer := tgbotapi.NewCallback(cq.ID, "")
		b.api.Request(answer) //nolint:errcheck
	}()

	data := cq.Data
	if !strings.HasPrefix(data, permCallbackPrefix) {
		return
	}
	data = strings.TrimPrefix(data, permCallbackPrefix)
	// data is now "<permissionID>:<reply>"
	lastColon := strings.LastIndex(data, ":")
	if lastColon < 0 {
		return
	}
	permID := data[:lastColon]
	reply := data[lastColon+1:]

	if reply != "once" && reply != "always" && reply != "reject" {
		return
	}

	b.permMu.Lock()
	perm, ok := b.pendingPerms[permID]
	if ok {
		delete(b.pendingPerms, permID)
	}
	b.permMu.Unlock()

	if !ok {
		b.log.Log("Received callback for unknown permission %s", permID)
		return
	}

	if err := b.oc.RespondToPermission(ctx, perm.sessionID, permID, reply); err != nil {
		b.log.Log("RespondToPermission error: %v", err)
		// Still update the message so the user sees feedback.
	}

	var label string
	switch reply {
	case "once":
		label = "✅ Approved (once)"
	case "always":
		label = "♾ Approved (always)"
	case "reject":
		label = "❌ Rejected"
	}

	removeKeyboard := tgbotapi.NewEditMessageReplyMarkup(perm.chatID, perm.msgID,
		tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}})
	b.api.Request(removeKeyboard) //nolint:errcheck

	resultMsg := tgbotapi.NewMessage(perm.chatID, label)
	b.api.Send(resultMsg) //nolint:errcheck
}

// isTimeoutError reports whether err was caused by a context deadline or
// cancellation (including when wrapped inside a url.Error from http).
func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

// sendReply sends a text reply to a message, splitting into multiple messages if needed.
func (b *Bot) sendReply(chatID int64, replyTo int, text string) *tgbotapi.Message {
	chunks := splitRunes(text, telegramMaxMessageLen)
	var firstSent *tgbotapi.Message

	for i, chunk := range chunks {
		msg := tgbotapi.NewMessage(chatID, chunk)
		if i == 0 {
			msg.ReplyToMessageID = replyTo
		}

		sent, err := b.api.Send(msg)
		if err != nil {
			b.log.Log("Send message error (chat=%d): %v", chatID, err)
			return firstSent
		}

		if firstSent == nil {
			firstSent = &sent
		}
	}

	return firstSent
}

// SendMessage sends a message to a chat (used by notify command).
// Long messages are automatically split at rune boundaries.
func SendMessage(botToken string, chatID int64, text string) error {
	api, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		return fmt.Errorf("bot init: %w", err)
	}
	for _, chunk := range splitRunes(text, telegramMaxMessageLen) {
		msg := tgbotapi.NewMessage(chatID, chunk)
		if _, err := api.Send(msg); err != nil {
			return err
		}
	}
	return nil
}

// lastUpdateIDFile returns the path to the last_update_id file.
func (b *Bot) lastUpdateIDFile() string {
	return filepath.Join(b.cfg.RuntimeDir, "last_update_id.txt")
}

// loadLastUpdateID reads the last processed update ID from disk.
func (b *Bot) loadLastUpdateID() int64 {
	data, err := os.ReadFile(b.lastUpdateIDFile())
	if err != nil {
		return 0
	}
	id, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// saveLastUpdateID persists the last processed update ID to disk.
func (b *Bot) saveLastUpdateID(updateID int64) {
	destFile := b.lastUpdateIDFile()
	tmpFile := destFile + ".tmp"
	if err := os.WriteFile(tmpFile, []byte(strconv.FormatInt(updateID, 10)), 0644); err != nil {
		b.log.Log("Failed to save last update id: %v", err)
		return
	}
	if err := os.Rename(tmpFile, destFile); err != nil {
		// On Windows, os.Rename fails if dest exists; remove and retry.
		if _, statErr := os.Stat(destFile); statErr == nil {
			if removeErr := os.Remove(destFile); removeErr != nil {
				b.log.Log("Failed to replace last update id file: %v", err)
				_ = os.Remove(tmpFile)
				return
			}
			if retryErr := os.Rename(tmpFile, destFile); retryErr != nil {
				b.log.Log("Failed to replace last update id file: %v", retryErr)
				_ = os.Remove(tmpFile)
				return
			}
		} else {
			b.log.Log("Failed to rename last update id file: %v", err)
			_ = os.Remove(tmpFile)
		}
	}
}

// truncateRunes returns the first n runes of s without splitting multi-byte characters.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// splitRunes splits s into chunks of at most n runes each, respecting rune boundaries.
func splitRunes(s string, n int) []string {
	runes := []rune(s)
	var chunks []string
	for len(runes) > 0 {
		end := n
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[:end]))
		runes = runes[end:]
	}
	return chunks
}
