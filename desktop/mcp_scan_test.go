package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/langbyyi/wxtap/desktop/internal/api"
	"github.com/langbyyi/wxtap/desktop/internal/mcp"
	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

// A mistyped path must not read as a clean 0-file scan.
func TestMCPScanRejectsMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := mcpScan(context.Background(), missing); err == nil {
		t.Fatal("missing directory must be an error")
	} else if !strings.Contains(err.Error(), "directory not found") {
		t.Fatalf("error: %v", err)
	}
}

func TestMCPScanScansExistingDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("var ak = 'AKIDEXAMPLE'"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := mcpScan(context.Background(), dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if result.FilesScanned != 1 {
		t.Fatalf("files scanned: %d", result.FilesScanned)
	}
}

// The MCP traffic page must marshal as an array, not null, so schema-validated
// clients can iterate an empty page.
func TestMCPTrafficListReturnsEmptyArray(t *testing.T) {
	repo, err := traffic.Open(resolveDBPath(), resolveMigrationsDir(exeDir()))
	if err != nil {
		t.Fatalf("open traffic db: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	adapter := &mcpTraffic{api: api.NewTrafficAPI(traffic.NewService(repo))}
	page, err := adapter.List(context.Background(), mcp.ListParams{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	data, err := json.Marshal(page)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"items":[]`) {
		t.Fatalf("empty page must marshal items as []: %s", data)
	}
}

func TestMCPScanReportIncludesStructuredFindings(t *testing.T) {
	dir := t.TempDir()
	const secret = "AKIDabcdefghijklmnop"
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte(`var ak = "`+secret+`";`), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := mcpScan(context.Background(), dir)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	var report struct {
		Findings []map[string]any `json:"findings"`
	}
	if err := json.Unmarshal(result.Report, &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if len(report.Findings) == 0 || report.Findings[0]["masked"] == "" {
		t.Fatalf("structured findings missing: %s", result.Report)
	}
}
