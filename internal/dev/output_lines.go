package dev

import (
	"bufio"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sync"
)

// Sink receives dev-process output. Lines mode prefixes by process name;
// panes mode routes each process to a separate terminal pane.
type Sink interface {
	Writer(name string) io.Writer
	SystemLine(format string, args ...any)
}

type LineSink struct {
	mu       sync.Mutex
	out      io.Writer
	useColor bool
}

func NewLineSink(out io.Writer, forceColor bool) *LineSink {
	return &LineSink{out: out, useColor: forceColor || isStdoutTTY(out)}
}

func (s *LineSink) Writer(name string) io.Writer {
	reader, writer := io.Pipe()
	prefix := s.colorPrefix(name)
	go func() {
		defer func() { _ = reader.Close() }()
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			s.mu.Lock()
			fmt.Fprintf(s.out, "%s %s\n", prefix, scanner.Text())
			s.mu.Unlock()
		}
	}()
	return writer
}

func (s *LineSink) SystemLine(format string, args ...any) {
	prefix := s.colorPrefix("angee")
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.out, "%s %s\n", prefix, fmt.Sprintf(format, args...))
}

var palette = []string{
	"\033[36m",
	"\033[33m",
	"\033[35m",
	"\033[32m",
	"\033[34m",
	"\033[31m",
	"\033[37m",
	"\033[90m",
}

func (s *LineSink) colorPrefix(name string) string {
	tag := "[" + name + "]"
	if !s.useColor {
		return tag
	}
	index := int(crc32.ChecksumIEEE([]byte(name))) % len(palette)
	return palette[index] + tag + "\033[0m"
}

func isStdoutTTY(w io.Writer) bool {
	if w != os.Stdout {
		return false
	}
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}
