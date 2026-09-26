package ipc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

func sampleExportItems() []map[string]any {
	return []map[string]any{
		{
			"appId":  "wx123",
			"type":   "callFunction",
			"name":   "login",
			"params": map[string]any{"a": 1},
			"status": "success",
			"time":   "12:00",
			"result": "ok",
		},
		{"appId": "wx456", "type": "db", "name": "users.get", "status": "fail", "time": "12:01"},
	}
}

func TestExportCloudXLSXWritesReportSheet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.xlsx")
	if err := ExportCloudXLSX(sampleExportItems(), path, nil); err != nil {
		t.Fatalf("export: %v", err)
	}

	f, err := excelize.OpenFile(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	sheet := "云函数审计报告"
	if len(f.GetSheetList()) == 0 || f.GetSheetList()[0] != sheet {
		t.Fatalf("sheet names: %v", f.GetSheetList())
	}
	headers := []string{"AppID", "类型", "名称", "参数", "状态", "时间", "结果"}
	for i, want := range headers {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		if got, _ := f.GetCellValue(sheet, cell); got != want {
			t.Fatalf("header %d: got %q want %q", i+1, got, want)
		}
	}
	if got, _ := f.GetCellValue(sheet, "A2"); got != "wx123" {
		t.Fatalf("A2: %q", got)
	}
	if got, _ := f.GetCellValue(sheet, "C3"); got != "users.get" {
		t.Fatalf("C3: %q", got)
	}
	// params dict is stored as indented JSON; missing result stays empty.
	if got, _ := f.GetCellValue(sheet, "D2"); !strings.Contains(got, "\"a\": 1") {
		t.Fatalf("D2 params: %q", got)
	}
	if got, _ := f.GetCellValue(sheet, "G3"); got != "" {
		t.Fatalf("G3 empty result: %q", got)
	}
	if width, err := f.GetColWidth(sheet, "D"); err != nil || width < 40 {
		t.Fatalf("col D width: %v %v", width, err)
	}
}

func TestExportCloudXLSXTruncatesAndWritesFullJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.xlsx")
	items := []map[string]any{
		{"appId": "wx1", "params": map[string]any{"big": strings.Repeat("x", 40000)}},
	}
	if err := ExportCloudXLSX(items, path, nil); err != nil {
		t.Fatalf("export: %v", err)
	}

	f, err := excelize.OpenFile(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	paramsCell, _ := f.GetCellValue("云函数审计报告", "D2")
	if len([]rune(paramsCell)) > excelCellLimit {
		t.Fatalf("params cell not truncated: %d runes", len([]rune(paramsCell)))
	}
	if !strings.Contains(paramsCell, "[截断，完整数据见同目录 .json 文件]") {
		t.Fatalf("truncation marker missing: %q", paramsCell)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	full, err := os.ReadFile(filepath.Join(dir, "report_full.json"))
	if err != nil {
		t.Fatalf("full json: %v", err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(full, &decoded); err != nil {
		t.Fatalf("parse full json: %v", err)
	}
	if len(decoded) != 1 {
		t.Fatalf("full json items: %d", len(decoded))
	}
}

func TestExportCloudXLSXProgressSteps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.xlsx")
	items := make([]map[string]any, 100)
	for i := range items {
		items[i] = map[string]any{"name": "n"}
	}
	var calls []int
	err := ExportCloudXLSX(items, path, func(current, total int) {
		calls = append(calls, current)
		if total != 100 {
			t.Errorf("total %d, want 100", total)
		}
	})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	// Step: total/20 → every 5th row plus the final row.
	if len(calls) != 20 || calls[len(calls)-1] != 100 {
		t.Fatalf("progress calls: %v", calls)
	}
}

// Live capture records carry data/timestamp; the export must prefer those
// keys and fall back to the params/time names.
func TestExportCloudXLSXPrefersDataAndTimestampKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.xlsx")
	items := []map[string]any{
		{
			"appId":     "wx123",
			"name":      "login",
			"data":      map[string]any{"user": "alice"},
			"status":    "success",
			"timestamp": "2026-09-20 12:00:00",
			"result":    "ok",
		},
		// Alternate keys still work when data/timestamp are absent.
		{"appId": "wx456", "name": "altKeys", "params": map[string]any{"a": 1}, "time": "12:01"},
	}
	if err := ExportCloudXLSX(items, path, nil); err != nil {
		t.Fatalf("export: %v", err)
	}

	f, err := excelize.OpenFile(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	sheet := "云函数审计报告"
	if got, _ := f.GetCellValue(sheet, "D2"); !strings.Contains(got, "\"user\": \"alice\"") {
		t.Fatalf("D2 should use data key: %q", got)
	}
	if got, _ := f.GetCellValue(sheet, "F2"); got != "2026-09-20 12:00:00" {
		t.Fatalf("F2 should use timestamp key: %q", got)
	}
	if got, _ := f.GetCellValue(sheet, "D3"); !strings.Contains(got, "\"a\": 1") {
		t.Fatalf("D3 should fall back to params: %q", got)
	}
	if got, _ := f.GetCellValue(sheet, "F3"); got != "12:01" {
		t.Fatalf("F3 should fall back to time: %q", got)
	}
}
