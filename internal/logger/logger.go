// Package logger provides UTC timestamped logging to both stdout and a file.
package logger

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// Logger writes timestamped log lines to stdout and an optional file.
type Logger struct {
	mu      sync.Mutex
	logFile string
}

// New creates a new Logger that writes to the given file path.
func New(logFile string) *Logger {
	return &Logger{logFile: logFile}
}

// Log writes a timestamped message to stdout and the log file.
func (l *Logger) Log(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	ts := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	line := fmt.Sprintf("[%s] %s", ts, msg)

	fmt.Println(line)

	if l.logFile == "" {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := os.OpenFile(l.logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}
