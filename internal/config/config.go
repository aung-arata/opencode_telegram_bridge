// Package config loads configuration from .env files and environment variables.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds all application configuration.
type Config struct {
	TGBotToken string
	TGUserID   int64
	TGChatID   int64

	OpenCodeURL            string
	OpenCodeSessionTimeout time.Duration

	RuntimeDir string
	LogFile    string
}

// Load reads configuration from the .env file and environment variables.
func Load() (*Config, error) {
	loadEnvFile(".env")

	botToken := os.Getenv("TG_BOT_TOKEN")
	if botToken == "" {
		return nil, fmt.Errorf("TG_BOT_TOKEN is required")
	}

	var (
		userID int64
		err    error
	)
	if userIDStr := os.Getenv("TG_USER_ID"); userIDStr != "" {
		userID, err = strconv.ParseInt(userIDStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("TG_USER_ID must be a number: %w", err)
		}
	}

	var chatID int64
	if s := os.Getenv("TG_CHAT_ID"); s != "" {
		chatID, err = strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("TG_CHAT_ID must be a number: %w", err)
		}
	}

	ocURL := os.Getenv("OPENCODE_URL")
	if ocURL == "" {
		ocURL = "http://127.0.0.1:4096"
	}

	sessionTimeout := 30 * time.Second
	if s := os.Getenv("OPENCODE_SESSION_TIMEOUT"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("OPENCODE_SESSION_TIMEOUT invalid duration: %w", err)
		}
		sessionTimeout = d
	}

	runtimeDir := "runtime"
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		return nil, fmt.Errorf("create runtime directory %q: %w", runtimeDir, err)
	}

	return &Config{
		TGBotToken:             botToken,
		TGUserID:               userID,
		TGChatID:               chatID,
		OpenCodeURL:            strings.TrimRight(ocURL, "/"),
		OpenCodeSessionTimeout: sessionTimeout,
		RuntimeDir:             runtimeDir,
		LogFile:                filepath.Join(runtimeDir, "oc_bridge.log"),
	}, nil
}

// loadEnvFile reads a .env file and sets environment variables (without overwriting).
func loadEnvFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])

		// Strip inline comments (preceded by whitespace + #)
		if idx := strings.Index(val, " #"); idx >= 0 {
			val = strings.TrimSpace(val[:idx])
		}

		// Unquote surrounding matching quotes
		if len(val) >= 2 && val[0] == val[len(val)-1] && (val[0] == '"' || val[0] == '\'') {
			val = val[1 : len(val)-1]
		}

		if key != "" {
			// Only set if not already in environment
			if _, exists := os.LookupEnv(key); !exists {
				os.Setenv(key, val)
			}
		}
	}
}
