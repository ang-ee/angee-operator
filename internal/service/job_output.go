package service

import (
	"fmt"
	"io"
	"sync"
)

// jobOutputSink serializes terminal writes from concurrent jobs and keeps
// status markers on their own lines. Writes are best effort: terminal output
// must never change the command result returned to the caller.
type jobOutputSink struct {
	mu       sync.Mutex
	writer   io.Writer
	lineOpen bool
}

func newJobOutputSink(writer io.Writer) *jobOutputSink {
	if writer == nil {
		return nil
	}
	return &jobOutputSink{writer: writer}
}

func (s *jobOutputSink) Write(p []byte) (int, error) {
	if s == nil {
		return len(p), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.writer.Write(p)
	if len(p) > 0 {
		s.lineOpen = p[len(p)-1] != '\n'
	}
	return len(p), nil
}

func (s *jobOutputSink) status(name, state string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lineOpen {
		_, _ = io.WriteString(s.writer, "\n")
	}
	_, _ = fmt.Fprintf(s.writer, "[job %s] %s\n", name, state)
	s.lineOpen = false
}
