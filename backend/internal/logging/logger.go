package logging

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Logger struct {
	mu     sync.Mutex
	logger *log.Logger
	writer *rotatingWriter
}

func New(logDir string) (*Logger, error) {
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, err
	}
	w := newRotatingWriter(filepath.Join(logDir, "app.log"), 16*1024*1024, 8)
	mw := io.MultiWriter(os.Stdout, w)
	return &Logger{
		logger: log.New(mw, "", 0),
		writer: w,
	}, nil
}

func (l *Logger) Close() error {
	return l.writer.Close()
}

func (l *Logger) Info(msg string, kv ...string) {
	l.log("INFO", msg, kv...)
}

func (l *Logger) Error(msg string, kv ...string) {
	l.log("ERROR", msg, kv...)
}

func (l *Logger) log(level, msg string, kv ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := time.Now().Format(time.RFC3339)
	line := fmt.Sprintf("%s level=%s msg=%q", ts, level, msg)
	for i := 0; i+1 < len(kv); i += 2 {
		line += fmt.Sprintf(" %s=%q", kv[i], kv[i+1])
	}
	l.logger.Println(line)
}
