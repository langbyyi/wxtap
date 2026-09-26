package extract

import (
	"fmt"
	"strings"
	"testing"
)

func TestFindingsFromAnalysisLocatesAndMasksCredential(t *testing.T) {
	content := `var safe = 1;` + "\n" + `// login configuration` + "\n" + `config = { "appsecret" = "a1b2c3d4e5f6g7h8" };` + "\n"
	analysis := Analysis{"secret": []string{`"appsecret" = "a1b2c3d4e5f6g7h8"`}}

	findings := FindingsFromAnalysis(analysis, content, "pages/login/login.js")
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1: %#v", len(findings), findings)
	}
	finding := findings[0]
	if finding.File != "pages/login/login.js" || finding.Line != 3 {
		t.Fatalf("location = %s:%d, want pages/login/login.js:3", finding.File, finding.Line)
	}
	if finding.Column <= 0 {
		t.Fatalf("column = %d, want a positive column", finding.Column)
	}
	if finding.Severity != "critical" || finding.Confidence != "high" {
		t.Fatalf("risk = %s/%s, want critical/high", finding.Severity, finding.Confidence)
	}
	if finding.Masked == "" || finding.Masked == finding.Value {
		t.Fatalf("credential must be masked in the default projection: %#v", finding)
	}
	if !strings.Contains(finding.Snippet, "appsecret") {
		t.Fatalf("snippet lost code context: %q", finding.Snippet)
	}
}

func TestFindingsFromAnalysisKeepsRepeatedLocations(t *testing.T) {
	content := "phone=15256535198; phone=15256535198;"
	findings := FindingsFromAnalysis(Analysis{"mobile": []string{"15256535198"}}, content, "chunk.js")
	if len(findings) != 2 {
		t.Fatalf("findings = %d, want 2: %#v", len(findings), findings)
	}
	if findings[0].Column == findings[1].Column {
		t.Fatalf("repeated matches must retain distinct columns: %#v", findings)
	}
}

func TestFindingsFromAnalysisFiltersPlaceholders(t *testing.T) {
	analysis := Analysis{
		"secret": []string{
			`"api_key" = "your_api_key_here"`,
			`"api_key" = "sk_live_1234567890abcdef"`,
		},
		"密码": []string{"示例密码"},
	}

	findings := FindingsFromAnalysis(analysis, "", "app.js")
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want only the real key: %#v", len(findings), findings)
	}
	if findings[0].Value != `"api_key" = "sk_live_1234567890abcdef"` {
		t.Fatalf("wrong key retained: %#v", findings[0])
	}
}

func TestFindingsFromAnalysisIncludesTargetMetadata(t *testing.T) {
	analysis := Analysis{
		"url": []string{"https://api.example.com/admin/users"},
		"oss": []string{"prod-assets.oss-cn-hangzhou.aliyuncs.com"},
	}

	findings := FindingsFromAnalysis(analysis, "", "services/api.js")
	if len(findings) != 2 {
		t.Fatalf("findings = %d, want 2", len(findings))
	}
	byCategory := map[string]Finding{}
	for _, finding := range findings {
		byCategory[finding.Category] = finding
	}
	if byCategory["url"].Severity != "high" || byCategory["url"].Privilege == "" {
		t.Fatalf("privileged endpoint metadata missing: %#v", byCategory["url"])
	}
	if byCategory["oss"].Severity != "medium" || byCategory["oss"].Category != "oss" {
		t.Fatalf("oss metadata missing: %#v", byCategory["oss"])
	}
}

func TestFindingsFromAnalysisIncludesStaticResources(t *testing.T) {
	findings := FindingsFromAnalysis(Analysis{"static": []string{"assets/logo.png"}}, "const logo = 'assets/logo.png'", "app.js")
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1: %#v", len(findings), findings)
	}
	if findings[0].Category != "static" || findings[0].Title != "静态资源路径" || findings[0].RuleID != "builtin:static" {
		t.Fatalf("static resource metadata missing: %#v", findings[0])
	}
}

func TestFindingsFromAnalysisKeepsCustomRuleMetadata(t *testing.T) {
	analysis := Analysis{"internal_ticket": []string{"OPS-2048-secret"}}
	findings := FindingsFromAnalysis(analysis, "", "custom.js")
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(findings))
	}
	if findings[0].RuleID != "custom:internal_ticket" || findings[0].Title != "自定义规则: internal_ticket" {
		t.Fatalf("custom metadata missing: %#v", findings[0])
	}
}

func TestFindingsFromAnalysisCapsTargetsAndKeepsCredentials(t *testing.T) {
	analysis := Analysis{"secret": []string{"sk_live_abcdef1234567890"}}
	for i := 0; i < maxFindingsPerFile+25; i++ {
		analysis["url"] = append(analysis["url"], fmt.Sprintf("https://api.example.com/item/%03d", i))
	}

	findings := FindingsFromAnalysis(analysis, "", "app.js")
	if len(findings) != maxFindingsPerFile {
		t.Fatalf("findings = %d, want cap %d", len(findings), maxFindingsPerFile)
	}
	if len(findings) == 0 || findings[0].Category != "secret" {
		t.Fatalf("credential was not prioritized: %#v", findings)
	}
}

func TestFindingsFromAnalysisPrioritizesCustomCredentialRules(t *testing.T) {
	analysis := Analysis{"wx_app_secret": []string{"abcdef1234567890"}}
	for i := 0; i < maxFindingsPerFile+25; i++ {
		analysis["url"] = append(analysis["url"], fmt.Sprintf("https://api.example.com/item/%03d", i))
	}

	findings := FindingsFromAnalysis(analysis, "", "app.js")
	found := false
	for _, finding := range findings {
		if finding.Category == "wx_app_secret" {
			found = true
			if finding.Severity != "high" || !hasFindingTag(finding, "credential") {
				t.Fatalf("custom credential metadata missing: %#v", finding)
			}
			break
		}
	}
	if !found {
		t.Fatalf("custom credential rule was dropped by target noise: %#v", findings)
	}
}

func hasFindingTag(finding Finding, tag string) bool {
	for _, value := range finding.Tags {
		if value == tag {
			return true
		}
	}
	return false
}

// 真实案例回归（iconfont 假阳性）：AKIA 格式命中浸泡在 WXSS 内嵌 base64
// 字体流里时，必须降级为 low 并注明原因——规则标题只保留「疑似」措辞。
func TestFindingsFromAnalysisDowngradesEncodedContextMatches(t *testing.T) {
	blob := strings.Repeat("WSUos0oqLCMiPQI+PW1u9bVoAwU0MaplYVdRYzIwUlVjV5IB", 12) + "AKIAAQAAAAAABQALAMMA" + strings.Repeat("AAAASAN4AAQAAAAAAAAAVACwAAQAAAAAABQALAMMA", 12)
	content := `var css = setCssToHead([".` + blob + `."]);` + "\n"
	analysis := Analysis{"secret": []string{"AKIAAQAAAAAABQALAMMA"}}

	findings := FindingsFromAnalysis(analysis, content, "components/mantisChat.wxss")
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(findings))
	}
	finding := findings[0]
	if finding.Confidence != "low" {
		t.Fatalf("编码流内的命中应降为 low: %s", finding.Confidence)
	}
	if finding.Note == "" {
		t.Fatal("降级必须带 note 说明原因")
	}
	if finding.Severity != "critical" {
		t.Fatalf("severity 表达「假如为真的影响」，不应随置信度变动: %s", finding.Severity)
	}
}

// 明文代码里的 AKIA 命中不受影响：离散字面量、邻居有引号与分隔符。
func TestFindingsFromAnalysisKeepsPlainLiteralConfidence(t *testing.T) {
	content := `var accessKeyId = "AKIAABCDEFGHIJKLMNOP";` + "\n"
	analysis := Analysis{"secret": []string{"AKIAABCDEFGHIJKLMNOP"}}

	findings := FindingsFromAnalysis(analysis, content, "config.js")
	if len(findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(findings))
	}
	if findings[0].Confidence != "high" {
		t.Fatalf("明文字面量应保持规则置信度: %s", findings[0].Confidence)
	}
	if findings[0].Note != "" {
		t.Fatalf("无降级不该有 note: %q", findings[0].Note)
	}
	if !strings.Contains(findings[0].Title, "疑似") {
		t.Fatalf("模式推断的标题应保留「疑似」: %q", findings[0].Title)
	}
}
