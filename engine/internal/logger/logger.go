package logger

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

var (
	levelNames = map[Level]string{
		LevelDebug: "DEBUG",
		LevelInfo:  "INFO",
		LevelWarn:  "WARN",
		LevelError: "ERROR",
	}

	botTokenRegex = regexp.MustCompile(`\b\d{8,11}:[a-zA-Z0-9_-]{30,}\b`)
	hashRegex     = regexp.MustCompile(`(?i)\b(?:hash|token|secret)[\s:=]+([a-zA-Z0-9_-]{16,})\b`)

	currentLevel = LevelInfo
	outMu        sync.Mutex
	outWriter    io.Writer = os.Stdout

	logListener func(string)
	listenerMu  sync.RWMutex
)

// SetListener registers a global callback for formatted, redacted log events.
func SetListener(fn func(string)) {
	listenerMu.Lock()
	defer listenerMu.Unlock()
	logListener = fn
}

// Redact sanitizes sensitive strings such as Bot Tokens and API Hashes.

func Redact(msg string) string {
	msg = botTokenRegex.ReplaceAllStringFunc(msg, func(s string) string {
		parts := strings.Split(s, ":")
		return parts[0] + ":[REDACTED]"
	})
	msg = hashRegex.ReplaceAllString(msg, "[REDACTED]")
	return msg
}

// Logger provides structured, leveled logging with redaction.
type Logger struct {
	prefix string
}

// New creates a new Logger instance.
func New(prefix string) *Logger {
	return &Logger{prefix: prefix}
}

func (l *Logger) log(lvl Level, msg string) {
	if lvl < currentLevel {
		return
	}

	cleanMsg := Redact(msg)

	timestamp := time.Now().Format("2006/01/02 15:04:05")
	formatted := fmt.Sprintf("%s [%-5s] [%s] %s", timestamp, levelNames[lvl], l.prefix, cleanMsg)

	outMu.Lock()
	fmt.Fprintln(outWriter, formatted)
	outMu.Unlock()

	listenerMu.RLock()
	listener := logListener
	listenerMu.RUnlock()
	if listener != nil {
		listener(formatted)
	}
}


func (l *Logger) Debugf(format string, args ...interface{}) {
	l.log(LevelDebug, fmt.Sprintf(format, args...))
}

func (l *Logger) Infof(format string, args ...interface{}) {
	l.log(LevelInfo, fmt.Sprintf(format, args...))
}

func (l *Logger) Warnf(format string, args ...interface{}) {
	l.log(LevelWarn, fmt.Sprintf(format, args...))
}

func (l *Logger) Errorf(format string, args ...interface{}) {
	l.log(LevelError, fmt.Sprintf(format, args...))
}
