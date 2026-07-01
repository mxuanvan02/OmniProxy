// Package logger provides a lightweight leveled logger for SuperKiro.
//
// Levels (from most to least verbose):
//
//	DEBUG < INFO < WARN < ERROR
//
// The active level is configured via logger.Init at startup.
// Priority: LOG_LEVEL environment variable > provided fallback (usually
// taken from config.json "logLevel"). If neither is set or the value is
// unrecognized, the level defaults to INFO.
package logger

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RingBuffer holds a fixed-capacity circular buffer of log entries.
type RingBuffer struct {
	mu    sync.Mutex
	lines []string
	cap   int
	pos   int
	full  bool
}

// NewRingBuffer creates a ring buffer with the given capacity.
func NewRingBuffer(capacity int) *RingBuffer {
	return &RingBuffer{lines: make([]string, capacity), cap: capacity}
}

// Write appends data to the ring buffer as a log line.
func (rb *RingBuffer) Write(p []byte) (int, error) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.lines[rb.pos] = string(p)
	rb.pos = (rb.pos + 1) % rb.cap
	if rb.pos == 0 {
		rb.full = true
	}
	return len(p), nil
}

// Lines returns all entries in chronological order.
func (rb *RingBuffer) Lines() []string {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if !rb.full {
		out := make([]string, rb.pos)
		copy(out, rb.lines[:rb.pos])
		return out
	}
	out := make([]string, rb.cap)
	copy(out, rb.lines[rb.pos:])
	copy(out[rb.cap-rb.pos:], rb.lines[:rb.pos])
	return out
}

// Level represents a log severity.
type Level int32

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

var (
	currentLevel atomic.Int32

	debugLog = log.New(os.Stdout, "DEBUG ", log.LstdFlags)
	infoLog  = log.New(os.Stdout, "INFO  ", log.LstdFlags)
	warnLog  = log.New(os.Stderr, "WARN  ", log.LstdFlags)
	errorLog = log.New(os.Stderr, "ERROR ", log.LstdFlags)

	// LogBuf stores the last 2048 log lines for the verbose log viewer.
	LogBuf = NewRingBuffer(2048)

	// logSubscribers receives every formatted log line as it is written, so the
	// web log viewer can live-tail. Kept in the logger package (not proxy) to
	// avoid an import cycle: proxy imports logger, so logger must not import proxy.
	logSubMu      sync.RWMutex
	logSubs       = map[int]func(string){}
	logSubNextID  int
)

// Subscribe registers fn to receive every log line written from now on. fn is
// invoked synchronously under a read lock from the logging goroutine, so it MUST
// be non-blocking (push to a buffered channel and return; never block or log).
// The returned function unregisters the subscriber and must be called to avoid
// a leak.
func Subscribe(fn func(string)) func() {
	logSubMu.Lock()
	id := logSubNextID
	logSubNextID++
	logSubs[id] = fn
	logSubMu.Unlock()
	return func() {
		logSubMu.Lock()
		delete(logSubs, id)
		logSubMu.Unlock()
	}
}

// notifyLog fans a formatted line out to all subscribers. Non-blocking is the
// subscriber's responsibility (see Subscribe).
func notifyLog(line string) {
	logSubMu.RLock()
	for _, fn := range logSubs {
		fn(line)
	}
	logSubMu.RUnlock()
}

// emit writes a formatted line to both the ring buffer and any live subscribers.
func emit(line string) {
	LogBuf.Write([]byte(line))
	notifyLog(line)
}

// logLine formats a message the same way the standard loggers do: prefix + date + message.
func logLine(prefix, format string, v ...interface{}) string {
	msg := fmt.Sprintf(format, v...)
	if len(msg) > 0 && msg[len(msg)-1] != '\n' {
		msg += "\n"
	}
	return prefix + time.Now().Format("2006/01/02 15:04:05 ") + msg
}

func init() {
	currentLevel.Store(int32(LevelInfo))
}

// ParseLevel converts a textual level ("debug", "info", "warn", "error")
// to a Level. The ok flag is false when the input is empty or unknown.
func ParseLevel(s string) (Level, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug", "trace":
		return LevelDebug, true
	case "info":
		return LevelInfo, true
	case "warn", "warning":
		return LevelWarn, true
	case "error", "err":
		return LevelError, true
	}
	return LevelInfo, false
}

// LevelName returns the canonical lowercase name of a Level.
func LevelName(l Level) string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	}
	return "info"
}

// SetLevel sets the active log level.
func SetLevel(l Level) {
	currentLevel.Store(int32(l))
}

// GetLevel returns the active log level.
func GetLevel() Level {
	return Level(currentLevel.Load())
}

// SetOutput redirects all level outputs to w. Useful for tests.
func SetOutput(w io.Writer) {
	debugLog.SetOutput(w)
	infoLog.SetOutput(w)
	warnLog.SetOutput(w)
	errorLog.SetOutput(w)
}

// Init configures the logger. The LOG_LEVEL environment variable, if set,
// overrides the supplied fallback (typically config.GetLogLevel()).
func Init(fallback string) {
	value := fallback
	if env := os.Getenv("LOG_LEVEL"); env != "" {
		value = env
	}
	if l, ok := ParseLevel(value); ok {
		SetLevel(l)
	}
}

func enabled(l Level) bool {
	return Level(currentLevel.Load()) <= l
}

// Debugf logs a formatted message at DEBUG level.
func Debugf(format string, v ...interface{}) {
	if enabled(LevelDebug) {
		debugLog.Printf(format, v...)
		emit(logLine("DEBUG ", format, v...))
	}
}

// Infof logs a formatted message at INFO level.
func Infof(format string, v ...interface{}) {
	if enabled(LevelInfo) {
		infoLog.Printf(format, v...)
		emit(logLine("INFO  ", format, v...))
	}
}

// Warnf logs a formatted message at WARN level.
func Warnf(format string, v ...interface{}) {
	if enabled(LevelWarn) {
		warnLog.Printf(format, v...)
		emit(logLine("WARN  ", format, v...))
	}
}

// Errorf logs a formatted message at ERROR level.
func Errorf(format string, v ...interface{}) {
	if enabled(LevelError) {
		errorLog.Printf(format, v...)
		emit(logLine("ERROR ", format, v...))
	}
}

// Fatalf logs a formatted message at ERROR level and terminates the process.
func Fatalf(format string, v ...interface{}) {
	errorLog.Printf(format, v...)
	emit(logLine("ERROR ", format, v...))
	os.Exit(1)
}
