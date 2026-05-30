package logger

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLog_WritesToFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	l := New(logPath)
	l.Log("hello %s", "world")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("log file not created: %v", err)
	}
	if !strings.Contains(string(data), "hello world") {
		t.Fatalf("log file missing message: %q", string(data))
	}
}

func TestLog_TimestampFormat(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	l := New(logPath)
	l.Log("ts check")

	data, _ := os.ReadFile(logPath)
	line := strings.TrimSpace(string(data))

	// Expect format: [2006-01-02T15:04:05Z] message
	if !strings.HasPrefix(line, "[") {
		t.Fatalf("expected line to start with '[', got: %q", line)
	}
	if !strings.Contains(line, "T") || !strings.Contains(line, "Z]") {
		t.Fatalf("timestamp does not look like ISO-8601 UTC: %q", line)
	}
}

func TestLog_AppendsMultipleLines(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	l := New(logPath)
	l.Log("line one")
	l.Log("line two")
	l.Log("line three")

	data, _ := os.ReadFile(logPath)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 log lines, got %d: %q", len(lines), string(data))
	}
}

func TestLog_EmptyPath_NoFilePanic(t *testing.T) {
	// logger.New("") must not panic or error — stdout only.
	l := New("")
	l.Log("no file, stdout only")
}

func TestLog_Concurrent(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "concurrent.log")
	l := New(logPath)

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(n int) {
			defer wg.Done()
			l.Log("goroutine %d", n)
		}(i)
	}
	wg.Wait()

	data, _ := os.ReadFile(logPath)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != goroutines {
		t.Fatalf("expected %d log lines, got %d", goroutines, len(lines))
	}
}

func TestLog_BadPath_NoUnhandledPanic(t *testing.T) {
	// Writing to an unwritable path should not panic — it emits a warning once.
	l := New("/nonexistent/dir/test.log")
	l.Log("should warn but not panic")
	l.Log("second call — fileErrOnce suppresses repeat warning")
}
