package ipc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/xuri/excelize/v2"
)

// excelCellLimit is Excel's hard character limit per cell; payloads beyond it
// are truncated in the sheet with a pointer to the companion _full.json.
const excelCellLimit = 32767

const truncationSuffix = "\n\n... [截断，完整数据见同目录 .json 文件]"

// ExportCloudXLSX writes the cloud-audit report: one
// sheet 云函数审计报告 with the seven report columns, blue header style,
// wrap-aligned long cells, report column widths, and progress callbacks at
// the total/20 step. When any params/result payload exceeds the Excel
// cell limit, a <stem>_full.json companion with the complete items is
// written next to the workbook. onProgress may be nil.
func ExportCloudXLSX(items []map[string]any, savePath string, onProgress func(current, total int)) error {
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()

	sheet := "云函数审计报告"
	if err := f.SetSheetName("Sheet1", sheet); err != nil {
		return fmt.Errorf("rename sheet: %w", err)
	}

	headerStyle, err := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "FFFFFF"},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"4472C4"}, Pattern: 1},
		Alignment: &excelize.Alignment{Horizontal: "center"},
	})
	if err != nil {
		return fmt.Errorf("header style: %w", err)
	}
	wrapStyle, err := f.NewStyle(&excelize.Style{
		Alignment: &excelize.Alignment{WrapText: true, Vertical: "top"},
	})
	if err != nil {
		return fmt.Errorf("wrap style: %w", err)
	}

	headers := []string{"AppID", "类型", "名称", "参数", "状态", "时间", "结果"}
	for i, h := range headers {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		if err := f.SetCellValue(sheet, cell, h); err != nil {
			return fmt.Errorf("header cell: %w", err)
		}
	}
	if err := f.SetCellStyle(sheet, "A1", "G1", headerStyle); err != nil {
		return fmt.Errorf("header style apply: %w", err)
	}

	total := len(items)
	progressStep := total / 20
	if progressStep < 1 {
		progressStep = 1
	}
	for idx, item := range items {
		row := idx + 2
		set := func(col int, value string) error {
			cell, _ := excelize.CoordinatesToCellName(col, row)
			if err := f.SetCellValue(sheet, cell, value); err != nil {
				return err
			}
			return nil
		}
		if err := set(1, exportText(item["appId"])); err != nil {
			return err
		}
		if err := set(2, exportText(item["type"])); err != nil {
			return err
		}
		if err := set(3, exportText(item["name"])); err != nil {
			return err
		}
		if err := set(5, exportText(item["status"])); err != nil {
			return err
		}
		// Live capture records carry data/timestamp; hand-built rows use the
		// params/time keys. Prefer the live names, fall back.
		if err := set(6, exportText(payloadOf(item, "timestamp", "time"))); err != nil {
			return err
		}
		paramsText := clampCell(exportText(payloadOf(item, "data", "params")))
		resultText := clampCell(exportText(item["result"]))
		if err := set(4, paramsText); err != nil {
			return err
		}
		if err := set(7, resultText); err != nil {
			return err
		}
		if err := f.SetCellStyle(sheet, fmt.Sprintf("D%d", row), fmt.Sprintf("D%d", row), wrapStyle); err != nil {
			return fmt.Errorf("wrap style apply: %w", err)
		}
		if err := f.SetCellStyle(sheet, fmt.Sprintf("G%d", row), fmt.Sprintf("G%d", row), wrapStyle); err != nil {
			return fmt.Errorf("wrap style apply: %w", err)
		}
		if onProgress != nil && ((idx+1)%progressStep == 0 || idx == total-1) {
			onProgress(idx+1, total)
		}
	}

	colWidths := map[string]float64{"A": 20, "B": 12, "C": 25, "D": 50, "E": 10, "F": 20, "G": 50}
	for col, width := range colWidths {
		if err := f.SetColWidth(sheet, col, col, width); err != nil {
			return fmt.Errorf("col width: %w", err)
		}
	}

	// SaveAs truncates its target before writing, so writing straight to the
	// user-chosen path would destroy the previous export and leave a corrupt
	// workbook behind on any failure (disk full, AV lock). Stage in a sibling
	// temp file (excelize picks the format from the extension, so it must
	// keep the .xlsx suffix) and move into place.
	ext := filepath.Ext(savePath)
	tmpPath := strings.TrimSuffix(savePath, ext) + ".wxtap-tmp" + ext
	if err := f.SaveAs(tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("save workbook: %w", err)
	}
	if err := os.Rename(tmpPath, savePath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace workbook: %w", err)
	}

	if hasOverflowingPayload(items) {
		fullPath := strings.TrimSuffix(savePath, filepath.Ext(savePath)) + "_full.json"
		full, err := marshalIndentNoEscape(items)
		if err != nil {
			return fmt.Errorf("marshal full json: %w", err)
		}
		if err := writeAtomic(fullPath, full); err != nil {
			return fmt.Errorf("write full json: %w", err)
		}
	}
	return nil
}

// exportText formats cell values: objects/arrays become indented
// JSON, strings pass through, nil is empty, everything else is JSON-encoded.
func exportText(v any) string {
	switch v.(type) {
	case nil:
		return ""
	case map[string]any, []any:
		if b, err := marshalIndentNoEscape(v); err == nil {
			return string(b)
		}
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := marshalNoEscape(v)
	return string(b)
}

// clampCell keeps the cell within Excel's limit, pointing at the _full.json.
func clampCell(text string) string {
	runes := []rune(text)
	if len(runes) <= excelCellLimit {
		return text
	}
	return string(runes[:excelCellLimit-80]) + truncationSuffix
}

// payloadOf returns the first non-nil value among the given keys (
// params/time rows vs. live data/timestamp rows).
func payloadOf(item map[string]any, keys ...string) any {
	for _, key := range keys {
		if v, ok := item[key]; ok && v != nil {
			return v
		}
	}
	return nil
}

// hasOverflowingPayload reports whether any params/result cell was clamped.
// The check must serialize exactly like the cell does — the indented
// exportText measured in runes (Excel's limit counts characters) — otherwise
// the truncation note can point at a _full.json that was never written.
func hasOverflowingPayload(items []map[string]any) bool {
	for _, item := range items {
		for _, v := range []any{payloadOf(item, "data", "params"), item["result"]} {
			if utf8.RuneCountInString(exportText(v)) > excelCellLimit {
				return true
			}
		}
	}
	return false
}

func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func marshalIndentNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// writeAtomic stages data in a sibling temp file and moves it into place, so
// a crash mid-write cannot leave a truncated file at the target path.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".wxtap-tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
