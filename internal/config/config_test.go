package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// loadEnvFile
// ---------------------------------------------------------------------------

func TestLoadEnvFile_SetsVars(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, ".env")
	os.WriteFile(f, []byte("TEST_KEY_A=hello\nTEST_KEY_B=world\n"), 0644)

	t.Setenv("TEST_KEY_A", "")
	t.Setenv("TEST_KEY_B", "")
	os.Unsetenv("TEST_KEY_A")
	os.Unsetenv("TEST_KEY_B")

	loadEnvFile(f)

	if got := os.Getenv("TEST_KEY_A"); got != "hello" {
		t.Fatalf("TEST_KEY_A: want %q, got %q", "hello", got)
	}
	if got := os.Getenv("TEST_KEY_B"); got != "world" {
		t.Fatalf("TEST_KEY_B: want %q, got %q", "world", got)
	}

	os.Unsetenv("TEST_KEY_A")
	os.Unsetenv("TEST_KEY_B")
}

func TestLoadEnvFile_DoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, ".env")
	os.WriteFile(f, []byte("TEST_OW=from_file\n"), 0644)

	t.Setenv("TEST_OW", "from_env")
	loadEnvFile(f)

	if got := os.Getenv("TEST_OW"); got != "from_env" {
		t.Fatalf("want %q (env wins), got %q", "from_env", got)
	}
}

func TestLoadEnvFile_SkipsComments(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, ".env")
	os.WriteFile(f, []byte("# this is a comment\n\nTEST_CMT=yes\n"), 0644)

	os.Unsetenv("TEST_CMT")
	loadEnvFile(f)
	if got := os.Getenv("TEST_CMT"); got != "yes" {
		t.Fatalf("want %q, got %q", "yes", got)
	}
	os.Unsetenv("TEST_CMT")
}

func TestLoadEnvFile_StripInlineComment(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, ".env")
	os.WriteFile(f, []byte("TEST_INLINE=value # inline comment\n"), 0644)

	os.Unsetenv("TEST_INLINE")
	loadEnvFile(f)
	if got := os.Getenv("TEST_INLINE"); got != "value" {
		t.Fatalf("want %q, got %q", "value", got)
	}
	os.Unsetenv("TEST_INLINE")
}

func TestLoadEnvFile_UnquotesDoubleQuotes(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, ".env")
	os.WriteFile(f, []byte(`TEST_DQ="quoted value"`+"\n"), 0644)

	os.Unsetenv("TEST_DQ")
	loadEnvFile(f)
	if got := os.Getenv("TEST_DQ"); got != "quoted value" {
		t.Fatalf("want %q, got %q", "quoted value", got)
	}
	os.Unsetenv("TEST_DQ")
}

func TestLoadEnvFile_UnquotesSingleQuotes(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, ".env")
	os.WriteFile(f, []byte("TEST_SQ='single'\n"), 0644)

	os.Unsetenv("TEST_SQ")
	loadEnvFile(f)
	if got := os.Getenv("TEST_SQ"); got != "single" {
		t.Fatalf("want %q, got %q", "single", got)
	}
	os.Unsetenv("TEST_SQ")
}

func TestLoadEnvFile_MissingFile(t *testing.T) {
	// Should not panic or return error — just silently skip.
	loadEnvFile("/nonexistent/path/.env")
}

// ---------------------------------------------------------------------------
// Load
// ---------------------------------------------------------------------------

// setupLoad changes to a temp directory (so runtime/ is created there) and
// sets the minimum required env vars. Returns a cleanup function.
func setupLoad(t *testing.T, extra map[string]string) {
	t.Helper()
	t.Chdir(t.TempDir())
	t.Setenv("TG_BOT_TOKEN", "test_token_123")
	for k, v := range extra {
		t.Setenv(k, v)
	}
}

func TestLoad_MinimalValid(t *testing.T) {
	setupLoad(t, nil)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TGBotToken != "test_token_123" {
		t.Fatalf("TGBotToken: want %q, got %q", "test_token_123", cfg.TGBotToken)
	}
	if cfg.OpenCodeURL != "http://127.0.0.1:4096" {
		t.Fatalf("default OpenCodeURL: got %q", cfg.OpenCodeURL)
	}
	if cfg.OpenCodeSessionTimeout != 30*time.Second {
		t.Fatalf("default timeout: got %v", cfg.OpenCodeSessionTimeout)
	}
	if cfg.RuntimeDir != "runtime" {
		t.Fatalf("RuntimeDir: got %q", cfg.RuntimeDir)
	}
}

func TestLoad_MissingToken(t *testing.T) {
	t.Chdir(t.TempDir())
	os.Unsetenv("TG_BOT_TOKEN")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error when TG_BOT_TOKEN is missing")
	}
}

func TestLoad_InvalidUserID(t *testing.T) {
	setupLoad(t, map[string]string{"TG_USER_ID": "not_a_number"})

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid TG_USER_ID")
	}
}

func TestLoad_InvalidChatID(t *testing.T) {
	setupLoad(t, map[string]string{"TG_CHAT_ID": "not_a_number"})

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid TG_CHAT_ID")
	}
}

func TestLoad_InvalidSessionTimeout(t *testing.T) {
	setupLoad(t, map[string]string{"OPENCODE_SESSION_TIMEOUT": "not_a_duration"})

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid OPENCODE_SESSION_TIMEOUT")
	}
}

func TestLoad_CustomValues(t *testing.T) {
	setupLoad(t, map[string]string{
		"TG_USER_ID":               "12345",
		"TG_CHAT_ID":               "67890",
		"OPENCODE_URL":             "http://localhost:9999/",
		"OPENCODE_SESSION_TIMEOUT": "1m",
	})

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TGUserID != 12345 {
		t.Fatalf("TGUserID: want 12345, got %d", cfg.TGUserID)
	}
	if cfg.TGChatID != 67890 {
		t.Fatalf("TGChatID: want 67890, got %d", cfg.TGChatID)
	}
	// Trailing slash should be stripped.
	if cfg.OpenCodeURL != "http://localhost:9999" {
		t.Fatalf("OpenCodeURL: want %q, got %q", "http://localhost:9999", cfg.OpenCodeURL)
	}
	if cfg.OpenCodeSessionTimeout != time.Minute {
		t.Fatalf("OpenCodeSessionTimeout: want 1m, got %v", cfg.OpenCodeSessionTimeout)
	}
}

func TestLoad_CreatesRuntimeDir(t *testing.T) {
	setupLoad(t, nil)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(cfg.RuntimeDir); os.IsNotExist(err) {
		t.Fatalf("runtime dir %q was not created", cfg.RuntimeDir)
	}
}
