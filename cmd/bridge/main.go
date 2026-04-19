// Command bridge is the OpenCode Telegram Bridge daemon.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/aung-arata/opencode-telegram-bridge/internal/config"
	"github.com/aung-arata/opencode-telegram-bridge/internal/logger"
	"github.com/aung-arata/opencode-telegram-bridge/internal/opencode"
	"github.com/aung-arata/opencode-telegram-bridge/internal/telegram"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	log := logger.New(cfg.LogFile)
	log.Log("OpenCode Telegram Bridge starting...")
	log.Log("OpenCode URL: %s", cfg.OpenCodeURL)
	log.Log("Session timeout: %s", cfg.OpenCodeSessionTimeout)

	oc := opencode.NewClient(cfg.OpenCodeURL, cfg.OpenCodeSessionTimeout, log)

	bot, err := telegram.NewBot(cfg, log, oc)
	if err != nil {
		log.Log("Fatal: %v", err)
		os.Exit(1)
	}

	// Graceful shutdown on SIGINT/SIGTERM
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		log.Log("Received signal %v, shutting down...", sig)
		cancel()
	}()

	bot.Run(ctx)
	log.Log("Bot stopped.")
}
