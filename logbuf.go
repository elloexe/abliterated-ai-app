package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A ring buffer of log lines for the live log viewer, mirrored to data/logs/app.log.

type logLine struct {
	Seq    int64  `json:"seq"`
	Time   string `json:"time"`
	Source string `json:"source"`
	Text   string `json:"text"`
}

var logs = &logBuffer{max: 4000}

type logBuffer struct {
	mu    sync.Mutex
	lines []logLine
	seq   int64
	max   int
	file  io.Writer
}

func initLogFile() {
	dir := filepath.Join(dataDir, "logs")
	_ = os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "app.log")
	// Keep one previous log around.
	if st, err := os.Stat(p); err == nil && st.Size() > 0 {
		_ = os.Rename(p, filepath.Join(dir, "app.prev.log"))
	}
	f, err := os.Create(p)
	if err == nil {
		logs.mu.Lock()
		logs.file = f
		logs.mu.Unlock()
	}
}

func (b *logBuffer) add(source, text string) {
	text = strings.TrimRight(text, "\r\n")
	if text == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	l := logLine{Seq: b.seq, Time: time.Now().Format("15:04:05"), Source: source, Text: text}
	b.lines = append(b.lines, l)
	if len(b.lines) > b.max {
		b.lines = append([]logLine(nil), b.lines[len(b.lines)-b.max:]...)
	}
	if b.file != nil {
		fmt.Fprintf(b.file, "%s [%s] %s\n", l.Time, source, text)
	}
}

func (b *logBuffer) since(seq int64) []logLine {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []logLine{}
	for _, l := range b.lines {
		if l.Seq > seq {
			out = append(out, l)
		}
	}
	return out
}

func logf(source, format string, args ...any) {
	logs.add(source, fmt.Sprintf(format, args...))
}

// lineWriter splits a process's output into lines for the log, and lets a
// callback inspect each line (used to spot fatal llama.cpp errors early).
type lineWriter struct {
	source string
	buf    []byte
	onLine func(string)
	mu     sync.Mutex
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		line := string(w.buf[:i])
		w.buf = w.buf[i+1:]
		logs.add(w.source, line)
		if w.onLine != nil {
			w.onLine(line)
		}
	}
	if len(w.buf) > 64*1024 {
		logs.add(w.source, string(w.buf))
		w.buf = nil
	}
	return len(p), nil
}
