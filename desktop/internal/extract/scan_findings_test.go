package extract

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanFilesEmitsLocatedFindingsForTextSources(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pages"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pages", "login.json"), []byte(`{"appsecret":"a1b2c3d4e5f6g7h8"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignore.bin"), []byte(`appsecret=a1b2c3d4e5f6g7h8`), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := ScanFilesWithPatterns(dir, nil)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(result.Findings) == 0 {
		t.Fatal("structured findings missing")
	}
	found := false
	for _, finding := range result.Findings {
		if finding.File == "pages/login.json" && finding.Category == "secret" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("JSON credential finding missing: %#v", result.Findings)
	}
	for _, finding := range result.Findings {
		if filepath.Ext(finding.File) == ".bin" {
			t.Fatalf("binary file must not be scanned: %#v", finding)
		}
	}
}

func TestScanFilesEmitsCustomPatternFindings(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte(`var ticket = "OPS-2048-secret";`), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := ScanFilesWithPatterns(dir, map[string]string{"internal_ticket": `OPS-[0-9]+-secret`})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(result.Findings) != 1 {
		t.Fatalf("findings = %d, want one custom finding: %#v", len(result.Findings), result.Findings)
	}
	if result.Findings[0].RuleID != "custom:internal_ticket" || result.Findings[0].File != "app.js" {
		t.Fatalf("custom finding metadata mismatch: %#v", result.Findings[0])
	}
}
