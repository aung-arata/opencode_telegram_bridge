// Package telegram implements the Telegram polling bot with command dispatch.
package telegram

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/aung-arata/opencode-telegram-bridge/internal/config"
	"github.com/aung-arata/opencode-telegram-bridge/internal/logger"
	"github.com/aung-arata/opencode-telegram-bridge/internal/opencode"
)

const (
	// telegramMaxMessageLen is the maximum text length Telegram allows in a single message.
	telegramMaxMessageLen = 4096
	// streamingTruncateLen is the limit for in-progress streaming edits (leaves room for suffix).
	streamingTruncateLen = 4000
	// minEditInterval throttles message edits to stay within Telegram's ~1 edit/second/chat rate limit.
	minEditInterval = 1 * time.Second
)

// Bot is the Telegram polling bot.
type Bot struct {
	api    *tgbotapi.BotAPI
	cfg    *config.Config
	log    *logger.Logger
	oc     *opencode.Client
	mu     sync.Mutex // serializes OpenCode queries
}

// NewBot creates a new Telegram bot instance.
func NewBot(cfg *config.Config, log *logger.Logger, oc *opencode.Client) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(cfg.TGBotToken)
	if err != nil {
		return nil, fmt.Errorf("telegram bot init: %w", err)
	}

	log.Log("Telegram bot authorized as @%s", api.Self.UserName)

	return &Bot{
		api: api,
		cfg: cfg,
		log: log,
		oc:  oc,
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
			if update.Message == nil {
				b.saveLastUpdateID(int64(update.UpdateID))
				continue
			}

			b.handleMessage(ctx, update.Message)
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
		"/ask <query> — explicit OpenCode query\n" +
		"/echo <msg>  — bot replies with your message\n" +
		"/help        — show this help\n\n" +
		fmt.Sprintf("OpenCode URL: %s\n", b.cfg.OpenCodeURL) +
		fmt.Sprintf("Session timeout: %s\n", b.cfg.OpenCodeSessionTimeout) +
		fmt.Sprintf("Log: %s", b.cfg.LogFile)
	b.sendReply(chatID, replyTo, help)
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
		if len(text) > streamingTruncateLen {
			text = text[:streamingTruncateLen] + "\n\n... (truncated, streaming)"
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

	response, err := b.oc.Query(ctx, chatID, query, onChunk)
	if err != nil {
		b.log.Log("OpenCode query error: %v", err)
		errMsg := fmt.Sprintf("❌ OpenCode error: %v", err)
		edit := tgbotapi.NewEditMessageText(chatID, placeholder.MessageID, errMsg)
		b.api.Send(edit)
		return
	}

	// Send the final response by editing the placeholder
	if len(response) > telegramMaxMessageLen {
		// Split into multiple messages if too long
		edit := tgbotapi.NewEditMessageText(chatID, placeholder.MessageID, response[:telegramMaxMessageLen])
		b.api.Send(edit)
		// Send remaining parts
		for i := telegramMaxMessageLen; i < len(response); i += telegramMaxMessageLen {
			end := i + telegramMaxMessageLen
			if end > len(response) {
				end = len(response)
			}
			b.sendReply(chatID, replyTo, response[i:end])
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

// sendReply sends a text reply to a message.
func (b *Bot) sendReply(chatID int64, replyTo int, text string) *tgbotapi.Message {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ReplyToMessageID = replyTo
	sent, err := b.api.Send(msg)
	if err != nil {
		b.log.Log("Send message error (chat=%d): %v", chatID, err)
		return nil
	}
	return &sent
}

// SendMessage sends a message to a chat (used by notify command).
func SendMessage(botToken string, chatID int64, text string) error {
	api, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		return fmt.Errorf("bot init: %w", err)
	}
	msg := tgbotapi.NewMessage(chatID, text)
	_, err = api.Send(msg)
	return err
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
	tmpFile := b.lastUpdateIDFile() + ".tmp"
	if err := os.WriteFile(tmpFile, []byte(strconv.FormatInt(updateID, 10)), 0644); err != nil {
		b.log.Log("Failed to save last update id: %v", err)
		return
	}
	if err := os.Rename(tmpFile, b.lastUpdateIDFile()); err != nil {
		b.log.Log("Failed to rename last update id file: %v", err)
	}
}
