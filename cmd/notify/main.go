// Command notify sends a one-shot message to a Telegram chat.
// Equivalent to the old send_telegram.py script.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/aung-arata/opencode-telegram-bridge/internal/config"
	"github.com/aung-arata/opencode-telegram-bridge/internal/telegram"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: notify MESSAGE...")
		os.Exit(1)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if cfg.TGChatID == 0 {
		fmt.Fprintln(os.Stderr, "Error: TG_CHAT_ID is required for notify command")
		os.Exit(1)
	}

	message := strings.Join(os.Args[1:], " ")

	if err := telegram.SendMessage(cfg.TGBotToken, cfg.TGChatID, message); err != nil {
		fmt.Fprintf(os.Stderr, "Error sending message: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Message sent successfully.")
}
