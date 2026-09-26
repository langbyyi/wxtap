package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The E2E gate asks operators to attach Core stderr logs as evidence, but the
// shell only kept a 32-line in-memory ring. Logs must survive the process so a
// failed attach can be diagnosed after the fact.
func TestOperationLogPersistsEntries(t *testing.T) {
	dir := t.TempDir()
	logger, err := openOperationLog(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	logger.Log("info", "engine.start ok")
	logger.Log("error", "attach failed")
	if err := logger.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatalf("logs dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one log file, got %d", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(dir, "logs", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"engine.start ok", "attach failed", "[info]", "[error]"} {
		if !strings.Contains(text, want) {
			t.Fatalf("log missing %q:\n%s", want, text)
		}
	}
	if name := entries[0].Name(); !strings.HasPrefix(name, "wxtap-") || !strings.HasSuffix(name, ".log") {
		t.Fatalf("unexpected log file name %q", name)
	}
}

// A runaway Core can produce megabytes of stderr; the file must rotate so a
// single session cannot fill the user's disk.
func TestOperationLogRotatesAtSizeLimit(t *testing.T) {
	dir := t.TempDir()
	logger, err := openOperationLog(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = logger.Close() }()

	line := strings.Repeat("x", 4096)
	for i := 0; i < 600; i++ { // ~2.4 MB, past the 1 MB rotation threshold
		logger.Log("info", line)
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 2 {
		t.Fatalf("expected a rotated file beside the active one, got %d", len(entries))
	}
}

func TestOperationLogDisabledWhenNoBaseDir(t *testing.T) {
	logger, err := openOperationLog("")
	if err != nil {
		t.Fatalf("no base dir must not fail: %v", err)
	}
	logger.Log("info", "dropped")
	if err := logger.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// The shell's own log events (engine lifecycle, Core stderr relay, failures)
// must reach the same file the operator inspects.
func TestEmitLogReachesOperationLog(t *testing.T) {
	dir := t.TempDir()
	logger, err := openOperationLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	app := NewApp()
	app.ctx = context.Background()
	app.dataBase = dir
	app.logger = logger
	app.emitLog("error", "core died")
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}

	path := newestLogFile(t, filepath.Join(dir, "logs"))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "core died") {
		t.Fatalf("emitLog did not persist: %s", data)
	}
}

func newestLogFile(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var newest string
	var newestTime time.Time
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if info.ModTime().After(newestTime) {
			newestTime = info.ModTime()
			newest = filepath.Join(dir, entry.Name())
		}
	}
	if newest == "" {
		t.Fatalf("no log file in %s", dir)
	}
	return newest
}

// After 99 rotations in one day the numbered suffixes run out; rotation must
// still move the oversized file aside instead of appending to it forever.
func TestOperationLogRotationSurvivesIndexExhaustion(t *testing.T) {
	dir := t.TempDir()
	logsDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logsDir, 0o750); err != nil {
		t.Fatal(err)
	}
	today := time.Now().Format("20060102")
	for index := 1; index <= 99; index++ {
		path := filepath.Join(logsDir, sprintf("wxtap-%s.%d.log", today, index))
		if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	logger, err := openOperationLog(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = logger.Close() }()
	line := strings.Repeat("y", 4096)
	for i := 0; i < 300; i++ {
		logger.Log("info", line)
	}

	active := filepath.Join(logsDir, sprintf("wxtap-%s.log", today))
	info, err := os.Stat(active)
	if err != nil {
		t.Fatalf("active log missing: %v", err)
	}
	if info.Size() >= logMaxBytes {
		t.Fatalf("active log was not rotated after index exhaustion: %d bytes", info.Size())
	}
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) <= 100 {
		t.Fatalf("expected the oversized file to be rotated aside, got %d files", len(entries))
	}
}

func sprintf(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

func TestOperationLogRotatesByDate(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2026, 9, 20, 23, 59, 59, 0, time.Local)
	logger, err := openOperationLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	logger.now = func() time.Time { return day }
	logger.Log("info", "old day")

	day = day.Add(time.Minute)
	logger.Log("info", "new day")
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}

	logsDir := filepath.Join(dir, "logs")
	oldData, err := os.ReadFile(filepath.Join(logsDir, "wxtap-20260920.log"))
	if err != nil || !strings.Contains(string(oldData), "old day") {
		t.Fatalf("old-day log missing: %v %q", err, oldData)
	}
	newData, err := os.ReadFile(filepath.Join(logsDir, "wxtap-20260921.log"))
	if err != nil || !strings.Contains(string(newData), "new day") {
		t.Fatalf("new-day log missing: %v %q", err, newData)
	}
}
