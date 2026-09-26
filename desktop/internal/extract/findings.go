package extract

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Finding is one located, classified scan result. Value is the raw evidence:
// the results panel shows it in its 匹配结果 column and the row copy hands it
// over for local triage. Masked is the shortened projection of the same value,
// kept for search matching and for any rendering that must stay redacted.
//
// 语义分工：Severity 是「假如为真，影响多大」（排序用）；Confidence 是「有多
// 大可能为真」（证据强度，逐条按上下文计算）；Title 只断言匹配到的东西的
// 字面类别，不断言其身份——身份要靠实测验证，扫描器给不了。
type Finding struct {
	ID         string `json:"id"`
	RuleID     string `json:"rule_id"`
	Category   string `json:"category"`
	Title      string `json:"title"`
	Severity   string `json:"severity"`
	Confidence string `json:"confidence"`
	Value      string `json:"value"`
	Masked     string `json:"masked"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	Column     int    `json:"column"`
	Snippet    string `json:"snippet,omitempty"`
	Privilege  string `json:"privilege,omitempty"`
	// Note carries per-occurrence context the rule could not know, e.g. why
	// the confidence was downgraded (a match inside an encoded data stream).
	Note string   `json:"note,omitempty"`
	Tags []string `json:"tags,omitempty"`
}

type findingMetadata struct {
	ruleID     string
	title      string
	severity   string
	confidence string
	privilege  string
	tags       []string
}

const maxFindingsPerFile = 300

var remainingFindingOrder = []string{
	"sfz", "mobile", "mail", "url", "path", "static", "jdbc",
}

// FindingsFromAnalysis converts the analysis category map into located findings.
// Missing source content is valid for callers that only need classification;
// location then falls back to line/column 1.
func FindingsFromAnalysis(analysis Analysis, content, file string) []Finding {
	if len(analysis) == 0 {
		return nil
	}

	keys := []string{"secret", "key", "jwt", "oss"}
	custom := make([]string, 0)
	for key := range analysis {
		if !isBuiltinCategory(key) {
			custom = append(custom, key)
		}
	}
	sort.Strings(custom)
	keys = append(keys, custom...)
	keys = append(keys, remainingFindingOrder...)

	seen := map[string]bool{}
	findings := make([]Finding, 0)
	for _, category := range keys {
		for _, rawValue := range analysis[category] {
			value := strings.TrimSpace(rawValue)
			if value == "" || isPlaceholderValue(category, value) {
				continue
			}
			meta := metadataForFinding(category, value)
			occurrences := locateFindings(content, value)
			for _, occurrence := range occurrences {
				if len(findings) >= maxFindingsPerFile {
					return findings
				}
				dedupeKey := category + "\x00" + value + "\x00" + strconv.Itoa(occurrence.line) + "\x00" + strconv.Itoa(occurrence.column)
				if seen[dedupeKey] {
					continue
				}
				seen[dedupeKey] = true
				confidence, note := occurrenceConfidence(meta, content, occurrence, value)
				findings = append(findings, Finding{
					ID:         findingID(category, file, occurrence.line, value+"\x00"+strconv.Itoa(occurrence.column)),
					RuleID:     meta.ruleID,
					Category:   category,
					Title:      meta.title,
					Severity:   meta.severity,
					Confidence: confidence,
					Value:      value,
					Masked:     maskValue(value),
					File:       file,
					Line:       occurrence.line,
					Column:     occurrence.column,
					Snippet:    occurrence.snippet,
					Privilege:  meta.privilege,
					Note:       note,
					Tags:       meta.tags,
				})
			}
		}
	}
	return findings
}

func metadataForFinding(category, value string) findingMetadata {
	switch category {
	case "secret":
		return secretMetadata(value)
	case "jwt":
		return findingMetadata{"builtin:jwt", "JWT 会话令牌", "high", "high", "可能重放用户或管理员会话", []string{"credential", "auth"}}
	case "oss":
		return findingMetadata{"builtin:oss", "云存储桶地址", "medium", "high", "可能读取或写入对象存储", []string{"storage", "cloud"}}
	case "url":
		privilege := privilegeForTarget(value)
		severity := "medium"
		if privilege != "" {
			severity = "high"
		}
		return findingMetadata{"builtin:url", "业务接口地址", severity, "high", privilege, []string{"target", "api"}}
	case "path":
		privilege := privilegeForTarget(value)
		severity := "low"
		if privilege != "" {
			severity = "high"
		}
		return findingMetadata{"builtin:path", "接口或资源路径", severity, "medium", privilege, []string{"target", "path"}}
	case "static":
		return findingMetadata{"builtin:static", "静态资源路径", "info", "high", "", []string{"asset", "static"}}
	case "sfz":
		return findingMetadata{"builtin:sfz", "身份证号码", "high", "medium", "个人信息泄露", []string{"pii", "identity"}}
	case "mobile":
		return findingMetadata{"builtin:mobile", "手机号码", "medium", "medium", "个人信息泄露", []string{"pii", "contact"}}
	case "mail":
		return findingMetadata{"builtin:mail", "邮箱地址", "low", "medium", "个人信息泄露", []string{"pii", "contact"}}
	case "jdbc":
		return findingMetadata{"builtin:jdbc", "JDBC 数据库地址", "medium", "high", "可能暴露数据库连接信息", []string{"database", "jdbc"}}
	case "key":
		return findingMetadata{"builtin:key", "密钥配置", "high", "medium", "可能访问关联接口或资源", []string{"credential", "key"}}
	default:
		lower := strings.ToLower(category)
		credentialLike := strings.Contains(lower, "key") || strings.Contains(lower, "token") ||
			strings.Contains(lower, "secret") || strings.Contains(lower, "password") ||
			strings.Contains(lower, "passwd") || strings.Contains(lower, "pwd") ||
			strings.Contains(category, "密码") || strings.Contains(category, "密钥") || strings.Contains(category, "令牌")
		if credentialLike {
			return findingMetadata{"custom:" + category, "自定义凭据规则: " + category, "high", "medium", "可能访问关联接口或资源", []string{"custom", "credential"}}
		}
		return findingMetadata{"custom:" + category, "自定义规则: " + category, "medium", "medium", "", []string{"custom"}}
	}
}

func secretMetadata(value string) findingMetadata {
	lower := strings.ToLower(value)
	switch {
	case strings.Contains(lower, "-----begin") && strings.Contains(lower, "private key"):
		// PEM 头是结构性证据：这段字节就是私钥材料，不是「长得像」。
		return findingMetadata{"secret:private_key", "私钥材料", "critical", "high", "可能导致身份或资源接管", []string{"credential", "private-key"}}
	case strings.Contains(lower, "appsecret") || strings.Contains(lower, "app_secret") ||
		strings.Contains(lower, "apiv3key") || strings.Contains(lower, "api_v3_key") ||
		strings.Contains(lower, "mch_key") || strings.Contains(lower, "paykey"):
		return findingMetadata{"secret:app_secret", "疑似应用或支付密钥", "critical", "high", "可能导致账号、支付或云资源接管", []string{"credential", "app-secret"}}
	case strings.Contains(lower, "akia") || strings.Contains(lower, "ltai") || strings.Contains(lower, "akid") ||
		strings.Contains(lower, "jdcloud") || strings.Contains(lower, "secretaccesskey"):
		return findingMetadata{"secret:cloud_ak", "疑似云平台访问密钥", "critical", "high", "可能导致云资源接管", []string{"credential", "cloud"}}
	case strings.Contains(lower, "bearer") || strings.Contains(lower, "authorization"):
		return findingMetadata{"secret:bearer", "疑似 Bearer 认证信息", "high", "high", "可能重放高权限会话", []string{"credential", "auth"}}
	case strings.Contains(lower, "password") || strings.Contains(lower, "passwd") || strings.Contains(lower, "pwd"):
		return findingMetadata{"secret:password", "疑似硬编码密码", "high", "high", "可能登录管理或业务后台", []string{"credential", "password"}}
	case strings.Contains(lower, "webhook") || strings.Contains(lower, "hooks.slack") || strings.Contains(lower, "qyapi.weixin"):
		// 完整 webhook URL（含协议、主机、路径、key 参数）是结构性证据。
		return findingMetadata{"secret:webhook", "Webhook 访问令牌", "high", "high", "可能向外部协作渠道发送消息", []string{"credential", "webhook"}}
	case strings.HasPrefix(lower, "sk_live_") || strings.Contains(lower, "sk-"):
		return findingMetadata{"secret:api_key", "疑似 API 密钥", "high", "high", "可能访问关联业务接口", []string{"credential", "api-key"}}
	default:
		return findingMetadata{"builtin:secret", "疑似硬编码凭据", "high", "medium", "可能访问关联接口或资源", []string{"credential"}}
	}
}

func privilegeForTarget(value string) string {
	lower := strings.ToLower(value)
	for _, keyword := range []string{"admin", "manage", "system", "upload", "delete", "remove", "update", "config", "export", "debug"} {
		if strings.Contains(lower, keyword) {
			return "路径包含高权限操作关键词"
		}
	}
	for _, keyword := range []string{"auth", "login", "token", "session", "user", "member", "pay", "order", "refund"} {
		if strings.Contains(lower, keyword) {
			return "路径与认证或核心业务权限相关"
		}
	}
	return ""
}

func isPlaceholderValue(category, value string) bool {
	lowerCategory := strings.ToLower(category)
	sensitiveCategory := category == "secret" || strings.HasPrefix(category, "custom") ||
		strings.Contains(lowerCategory, "key") || strings.Contains(lowerCategory, "token") ||
		strings.Contains(lowerCategory, "secret") || strings.Contains(category, "密码") ||
		strings.Contains(category, "密钥") || strings.Contains(category, "令牌")
	if !sensitiveCategory {
		return false
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{
		"your_", "your-", "example", "placeholder", "changeme", "change_me", "replace_me",
		"dummy", "sample", "${", "{{", "<your", "xxxxx", "abcd1234",
		"示例", "测试", "请填入", "请替换", "请输入",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

type findingOccurrence struct {
	line, column int
	snippet      string
}

// encodedNote is attached to findings whose match sits inside a long encoded
// (base64-shaped) run — font/image/config payloads embedded as data URIs.
const encodedNote = "命中位于 base64/编码数据流内部，大概率为编码字节的随机碰撞，不是明文凭据"

// occurrenceConfidence regrades the rule-level confidence for one occurrence.
// A real credential sits in code as a discrete literal: quotes, separators or
// spaces within a few characters. A match whose neighbors run on for dozens
// of base64-alphabet bytes on both sides is in the middle of an encoded blob
// (the iconfont-in-WXSS case), and reads low confidence no matter how scary
// the rule looks.
func occurrenceConfidence(meta findingMetadata, content string, occurrence findingOccurrence, value string) (string, string) {
	if meta.confidence == "low" {
		return meta.confidence, ""
	}
	if !inEncodedRun(content, occurrence, value) {
		return meta.confidence, ""
	}
	return "low", encodedNote
}

// inEncodedRun reports whether the match is engulfed by a single uninterrupted
// base64-alphabet run of at least encodedRunMinLength bytes (match included).
const encodedRunMinLength = 100

func inEncodedRun(content string, occurrence findingOccurrence, value string) bool {
	if content == "" || value == "" {
		return false
	}
	// Byte offset of the match: count back occurrence.line-1 newlines, then
	// occurrence.column-1 runes into the line. Reconstructing the offset beats
	// threading it through locateFindings for the one caller that needs it.
	offset := 0
	for i, line1 := 1, occurrence.line; i < line1 && offset < len(content); i++ {
		if next := strings.Index(content[offset:], "\n"); next >= 0 {
			offset += next + 1
		} else {
			offset = len(content)
		}
	}
	offset += len(string([]rune(content[offset:])[:minRunes(occurrence.column-1, content[offset:])]))
	end := offset + len(value)
	isB64 := func(b byte) bool {
		return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '+' || b == '/' || b == '=' || b == '-' || b == '_'
	}
	left, right := 0, 0
	for i := offset - 1; i >= 0 && isB64(content[i]); i-- {
		left++
	}
	for i := end; i < len(content) && isB64(content[i]); i++ {
		right++
	}
	return left+right+len(value) >= encodedRunMinLength
}

func minRunes(n int, s string) int {
	if n <= 0 {
		return 0
	}
	count := 0
	for range s {
		if count >= n {
			break
		}
		count++
	}
	return count
}

func locateFindings(content, value string) []findingOccurrence {
	if content == "" || value == "" {
		return []findingOccurrence{{line: 1, column: 1}}
	}
	occurrences := make([]findingOccurrence, 0, 1)
	from := 0
	for from <= len(content)-len(value) {
		index := strings.Index(content[from:], value)
		if index < 0 {
			break
		}
		index += from
		line, column, snippet := locateFindingAt(content, index)
		occurrences = append(occurrences, findingOccurrence{line: line, column: column, snippet: snippet})
		from = index + len(value)
	}
	if len(occurrences) == 0 {
		return []findingOccurrence{{line: 1, column: 1}}
	}
	return occurrences
}

func locateFindingAt(content string, index int) (int, int, string) {
	line := 1 + strings.Count(content[:index], "\n")
	lineStart := strings.LastIndex(content[:index], "\n") + 1
	lineEnd := strings.Index(content[index:], "\n")
	if lineEnd < 0 {
		lineEnd = len(content)
	} else {
		lineEnd += index
	}
	snippet := strings.TrimSpace(content[lineStart:lineEnd])
	if len(snippet) > 500 {
		snippet = snippet[:500] + "..."
	}
	column := utf8.RuneCountInString(content[lineStart:index]) + 1
	return line, column, snippet
}

func findingID(category, file string, line int, value string) string {
	sum := sha256.Sum256([]byte(category + "\x00" + file + "\x00" + strconv.Itoa(line) + "\x00" + value))
	return hex.EncodeToString(sum[:8])
}

func maskValue(value string) string {
	runes := []rune(value)
	if len(runes) <= 6 {
		return strings.Repeat("*", len(runes))
	}
	prefix := 4
	suffix := 4
	if len(runes) < 12 {
		prefix = 2
		suffix = 2
	}
	return string(runes[:prefix]) + "..." + string(runes[len(runes)-suffix:])
}
