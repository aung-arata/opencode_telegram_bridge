package telegram

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aung-arata/opencode-telegram-bridge/internal/config"
	"github.com/aung-arata/opencode-telegram-bridge/internal/logger"
	"github.com/aung-arata/opencode-telegram-bridge/internal/opencode"
)

// ---------------------------------------------------------------------------
// truncateRunes
// ---------------------------------------------------------------------------

func TestTruncateRunes_ShorterThanLimit(t *testing.T) {
	if got := truncateRunes("hello", 10); got != "hello" {
		t.Fatalf("want %q, got %q", "hello", got)
	}
}

func TestTruncateRunes_ExactLimit(t *testing.T) {
	if got := truncateRunes("hello", 5); got != "hello" {
		t.Fatalf("want %q, got %q", "hello", got)
	}
}

func TestTruncateRunes_ExceedsLimit(t *testing.T) {
	if got := truncateRunes("hello world", 5); got != "hello" {
		t.Fatalf("want %q, got %q", "hello", got)
	}
}

func TestTruncateRunes_MultiByte(t *testing.T) {
	// "日本語" = 3 runes; truncating to 2 should give "日本", not a broken byte sequence.
	got := truncateRunes("日本語", 2)
	if got != "日本" {
		t.Fatalf("want %q, got %q", "日本", got)
	}
}

func TestTruncateRunes_Empty(t *testing.T) {
	if got := truncateRunes("", 5); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// splitRunes
// ---------------------------------------------------------------------------

func TestSplitRunes_NoSplit(t *testing.T) {
	chunks := splitRunes("hello", 10)
	if len(chunks) != 1 || chunks[0] != "hello" {
		t.Fatalf("expected single chunk %q, got %v", "hello", chunks)
	}
}

func TestSplitRunes_ExactChunkSize(t *testing.T) {
	chunks := splitRunes("abcde", 5)
	if len(chunks) != 1 || chunks[0] != "abcde" {
		t.Fatalf("want 1 chunk, got %v", chunks)
	}
}

func TestSplitRunes_SplitsEvenly(t *testing.T) {
	chunks := splitRunes("abcdef", 2)
	want := []string{"ab", "cd", "ef"}
	if len(chunks) != len(want) {
		t.Fatalf("want %v, got %v", want, chunks)
	}
	for i, w := range want {
		if chunks[i] != w {
			t.Fatalf("chunk[%d]: want %q, got %q", i, w, chunks[i])
		}
	}
}

func TestSplitRunes_SplitsWithRemainder(t *testing.T) {
	chunks := splitRunes("abcdefg", 3)
	want := []string{"abc", "def", "g"}
	if len(chunks) != len(want) {
		t.Fatalf("want %v, got %v", want, chunks)
	}
	for i, w := range want {
		if chunks[i] != w {
			t.Fatalf("chunk[%d]: want %q, got %q", i, w, chunks[i])
		}
	}
}

func TestSplitRunes_MultiByteRunes(t *testing.T) {
	// "日本語テスト" = 6 runes; split at 3 should give ["日本語", "テスト"].
	chunks := splitRunes("日本語テスト", 3)
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "日本語" || chunks[1] != "テスト" {
		t.Fatalf("unexpected chunks: %v", chunks)
	}
}

func TestSplitRunes_Empty(t *testing.T) {
	chunks := splitRunes("", 5)
	if len(chunks) != 0 {
		t.Fatalf("expected no chunks for empty string, got %v", chunks)
	}
}

func TestSplitRunes_ReassemblyPreservesContent(t *testing.T) {
	original := strings.Repeat("あいうえお", 100) // 500 runes
	chunks := splitRunes(original, 60)
	if strings.Join(chunks, "") != original {
		t.Fatal("reassembled chunks do not match original")
	}
}

// ---------------------------------------------------------------------------
// loadLastUpdateID / saveLastUpdateID
// ---------------------------------------------------------------------------

// botForUpdateIDTests constructs a minimal Bot with only the fields needed
// by loadLastUpdateID / saveLastUpdateID (no live Telegram API required).
func botForUpdateIDTests(t *testing.T) *Bot {
	t.Helper()
	dir := t.TempDir()
	return &Bot{
		cfg: &config.Config{RuntimeDir: dir},
		log: logger.New(""),
	}
}

func TestLoadLastUpdateID_MissingFile(t *testing.T) {
	b := botForUpdateIDTests(t)
	if id := b.loadLastUpdateID(); id != 0 {
		t.Fatalf("want 0 for missing file, got %d", id)
	}
}

func TestSaveAndLoadLastUpdateID_RoundTrip(t *testing.T) {
	b := botForUpdateIDTests(t)

	b.saveLastUpdateID(12345)
	if id := b.loadLastUpdateID(); id != 12345 {
		t.Fatalf("want 12345, got %d", id)
	}
}

func TestSaveLastUpdateID_Overwrite(t *testing.T) {
	b := botForUpdateIDTests(t)

	b.saveLastUpdateID(1)
	b.saveLastUpdateID(999)
	if id := b.loadLastUpdateID(); id != 999 {
		t.Fatalf("want 999 after overwrite, got %d", id)
	}
}

func TestLoadLastUpdateID_CorruptFile(t *testing.T) {
	b := botForUpdateIDTests(t)

	// Write non-numeric content — should return 0 gracefully.
	os.WriteFile(filepath.Join(b.cfg.RuntimeDir, "last_update_id.txt"), []byte("not_a_number"), 0644)
	if id := b.loadLastUpdateID(); id != 0 {
		t.Fatalf("want 0 for corrupt file, got %d", id)
	}
}

func TestLoadLastUpdateID_WhitespaceTrimmed(t *testing.T) {
	b := botForUpdateIDTests(t)

	os.WriteFile(filepath.Join(b.cfg.RuntimeDir, "last_update_id.txt"),
		[]byte("  42\n"), 0644)
	if id := b.loadLastUpdateID(); id != 42 {
		t.Fatalf("want 42, got %d", id)
	}
}

func TestSaveLastUpdateID_FileContents(t *testing.T) {
	b := botForUpdateIDTests(t)
	b.saveLastUpdateID(777)

	raw, err := os.ReadFile(filepath.Join(b.cfg.RuntimeDir, "last_update_id.txt"))
	if err != nil {
		t.Fatalf("reading saved file: %v", err)
	}
	if strings.TrimSpace(string(raw)) != "777" {
		t.Fatalf("want file content %q, got %q", "777", string(raw))
	}
}

func TestSaveLastUpdateID_SequentialIDs(t *testing.T) {
	b := botForUpdateIDTests(t)

	for i := int64(1); i <= 10; i++ {
		b.saveLastUpdateID(i)
		if got := b.loadLastUpdateID(); got != i {
			t.Fatalf("after save(%d): want %d, got %d", i, i, got)
		}
	}
}

func TestSaveLastUpdateID_LargeID(t *testing.T) {
	b := botForUpdateIDTests(t)
	large := int64(9_999_999_999)
	b.saveLastUpdateID(large)

	raw, _ := os.ReadFile(filepath.Join(b.cfg.RuntimeDir, "last_update_id.txt"))
	if strings.TrimSpace(string(raw)) != strconv.FormatInt(large, 10) {
		t.Fatalf("large ID not stored correctly: %q", string(raw))
	}
	if got := b.loadLastUpdateID(); got != large {
		t.Fatalf("want %d, got %d", large, got)
	}
}

// ---------------------------------------------------------------------------
// lastUserMessageID
// ---------------------------------------------------------------------------

func TestLastUserMessageID_Empty(t *testing.T) {
	if got := lastUserMessageID(nil); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

func TestLastUserMessageID_NoUserMessages(t *testing.T) {
	msgs := []opencode.MessageInfo{
		{ID: "a1", Role: "assistant"},
		{ID: "a2", Role: "assistant"},
	}
	if got := lastUserMessageID(msgs); got != "" {
		t.Fatalf("want empty when no user messages, got %q", got)
	}
}

func TestLastUserMessageID_PicksLast(t *testing.T) {
	msgs := []opencode.MessageInfo{
		{ID: "u1", Role: "user"},
		{ID: "a1", Role: "assistant"},
		{ID: "u2", Role: "user"},
		{ID: "a2", Role: "assistant"},
	}
	if got := lastUserMessageID(msgs); got != "u2" {
		t.Fatalf("want u2, got %q", got)
	}
}

func TestLastUserMessageID_LastIsAssistant(t *testing.T) {
	msgs := []opencode.MessageInfo{
		{ID: "u1", Role: "user"},
		{ID: "a1", Role: "assistant"},
	}
	if got := lastUserMessageID(msgs); got != "u1" {
		t.Fatalf("want u1 (last user msg), got %q", got)
	}
}

func TestLastUserMessageID_OnlyOneUserMessage(t *testing.T) {
	msgs := []opencode.MessageInfo{
		{ID: "u1", Role: "user"},
	}
	if got := lastUserMessageID(msgs); got != "u1" {
		t.Fatalf("want u1, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// isDangerous / containsDangerousCommand
// ---------------------------------------------------------------------------

func TestIsDangerous_ExternalDirectory(t *testing.T) {
	req := opencode.PermissionRequest{Permission: "external_directory", Patterns: []string{"/*"}}
	if !isDangerous(req) {
		t.Fatal("external_directory should always be dangerous")
	}
}

func TestIsDangerous_UnknownTool_AutoApproved(t *testing.T) {
	req := opencode.PermissionRequest{Permission: "read_file", Patterns: []string{"/tmp/foo"}}
	if isDangerous(req) {
		t.Fatal("unknown tool should be auto-approved (not dangerous)")
	}
}

func TestIsDangerous_BashSafeCommands(t *testing.T) {
	safe := []string{
		"ls /",
		"mkdir /test_server",
		"cat /etc/hosts",
		"grep -r foo .",
		"find / -name '*.go'",
		"echo hello",
		"pwd",
	}
	for _, p := range safe {
		req := opencode.PermissionRequest{Permission: "bash", Patterns: []string{p}}
		if isDangerous(req) {
			t.Errorf("expected safe, got dangerous for pattern %q", p)
		}
	}
}

func TestIsDangerous_BashDangerousCommands(t *testing.T) {
	dangerous := []string{
		"rm -rf /",
		"rmdir /tmp/stuff",
		"dd if=/dev/zero of=/dev/sda",
		"shred /dev/sda",
		"truncate -s 0 important.db",
		"mkfs.ext4 /dev/sdb",
		"/bin/rm foo",
		"ls | rm -rf",
	}
	for _, p := range dangerous {
		req := opencode.PermissionRequest{Permission: "bash", Patterns: []string{p}}
		if !isDangerous(req) {
			t.Errorf("expected dangerous, got safe for pattern %q", p)
		}
	}
}

func TestIsDangerous_BashMultiplePatterns_AnyDangerous(t *testing.T) {
	req := opencode.PermissionRequest{
		Permission: "bash",
		Patterns:   []string{"ls /", "rm -rf /tmp/work"},
	}
	if !isDangerous(req) {
		t.Fatal("should be dangerous when any pattern is dangerous")
	}
}

