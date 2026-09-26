package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/update"
)

// logMaxBytes rotates the active file once it grows past ~1 MB: a noisy Core
// must not be able to fill the user's disk in one session.
const logMaxBytes = 1 << 20

// operationLog appends shell and Core log lines to
// <base>/logs/wxtap-YYYYMMDD.log, rotating the previous file to
// wxtap-YYYYMMDD.<n>.log. Every method is safe for concurrent use.
type operationLog struct {
	mu      sync.Mutex
	dir     string
	file    *os.File
	written int64
	now     func() time.Time
}

// openOperationLog creates the logger. An empty base (no user config dir)
// yields a disabled logger instead of an error: logging must never be the
// reason the app fails to start.
func openOperationLog(base string) (*operationLog, error) {
	logger := &operationLog{now: time.Now}
	if strings.TrimSpace(base) == "" {
		return logger, nil
	}
	logger.dir = filepath.Join(base, "logs")
	if err := os.MkdirAll(logger.dir, 0o750); err != nil {
		return nil, fmt.Errorf("创建日志目录失败: %w", err)
	}
	if err := logger.openLocked(); err != nil {
		return nil, err
	}
	return logger, nil
}

// logApplyRecord reports the outcome of the swap the previous process
// performed. applyStagedUpdate runs before any logging exists, so it leaves a
// record on disk for this launch to fold into the log — and to consume, so an
// update is reported exactly once.
func logApplyRecord(logger *operationLog, base string) {
	record, ok := update.LastApply(base)
	if !ok {
		return
	}
	update.ClearLastApply(base)
	level, message := record.Summary()
	logger.Log(level, message)
}

// Log appends one line; errors are swallowed because a logging failure must
// not take down the operation being logged.
func (l *operationLog) Log(level, message string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return
	}
	now := l.now()
	// The file name is date-based. A long-running process must switch to the
	// new day's file instead of appending yesterday's name forever.
	if filepath.Base(l.file.Name()) != filepath.Base(l.activePath(now)) {
		_ = l.file.Close()
		l.file = nil
		if err := l.openLockedAt(now); err != nil {
			return
		}
	}
	line := fmt.Sprintf("%s [%s] %s\n", now.Format("2006-01-02 15:04:05.000"), level, message)
	n, err := l.file.WriteString(line)
	if err != nil {
		return
	}
	l.written += int64(n)
	if l.written >= logMaxBytes {
		_ = l.file.Close()
		l.file = nil
		if err := l.rotateLocked(); err != nil {
			return
		}
	}
}

// Close flushes and releases the active file.
func (l *operationLog) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

func (l *operationLog) activePath(t time.Time) string {
	return filepath.Join(l.dir, "wxtap-"+t.Format("20060102")+".log")
}

func (l *operationLog) openLocked() error {
	return l.openLockedAt(l.now())
}

func (l *operationLog) openLockedAt(now time.Time) error {
	path := l.activePath(now)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("打开日志文件失败: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("读取日志大小失败: %w", err)
	}
	l.file = file
	l.written = info.Size()
	return nil
}

// rotateLocked renames the current file aside and opens a fresh one. Numbered
// suffixes are used first; once they are exhausted (a pathological day) a
// timestamp suffix keeps rotation working instead of reopening the oversized
// file.
func (l *operationLog) rotateLocked() error {
	now := l.now()
	active := ""
	if l.file != nil {
		active = l.file.Name()
	} else {
		active = l.activePath(now)
	}
	stem := strings.TrimSuffix(active, ".log")
	renamed := false
	for index := 1; index < 100; index++ {
		rotated := fmt.Sprintf("%s.%d.log", stem, index)
		if _, err := os.Stat(rotated); os.IsNotExist(err) {
			if err := os.Rename(active, rotated); err != nil {
				return err
			}
			renamed = true
			break
		}
	}
	if !renamed {
		rotated := fmt.Sprintf("%s.%d.log", stem, now.UnixNano())
		if err := os.Rename(active, rotated); err != nil {
			return err
		}
	}
	return l.openLocked()
}
