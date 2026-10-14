package mcp

// MCP 动态面默认打码：hook_drain / storage / console / 流量读面的回包是 agent
// 上下文的直接输入，登录态、证件、手机号落进 transcript 就拿不回来。打码只在
// MCP 边界做——GUI 与实际重放报文保持捕获原文（ak.go「a masked packet is not
// the packet」的立场属于人工验活面，两者不冲突）。

import (
	"encoding/json"
	"regexp"
	"strings"
)

// sensitiveKeyFragments is the key-name rule table (case-insensitive
// substrings). Keys and structure survive — the schema around a redacted value
// is what an agent navigates by, so only the value collapses.
var sensitiveKeyFragments = []string{
	"token", "cookie", "authorization", "auth", "session", "ticket",
	"openid", "unionid", "phone", "mobile", "idcard", "password", "passwd",
	"secret", "api_key", "apikey", "access_key", "skey", "credential", "set-cookie",
}

// 身份形状的两条值规则：没有敏感键名兜底的载体（塞进 userName 里的手机号、
// URL query）靠它们兜底。手机号带数字边界；证件以整段 run 校验——它以 X 结尾，
// 单字符后向边界拦不住更长的数字串被劈出假命中。
var (
	phonePattern    = regexp.MustCompile(`(^|[^\d])(1[3-9]\d{9})([^\d]|$)`)
	digitRunPattern = regexp.MustCompile(`[\dXx]+`)
	idShapePattern  = regexp.MustCompile(`^\d{17}[\dXx]$`)
)

const (
	redactMaxDepth = 12
	redactMaxNodes = 20000
)

// redaction carries the walk budget and the change counter.
type redaction struct{ nodes, changes int }

// redactAny masks sensitive leaves of a decoded JSON value: key-name rules
// win, then the value-shape rules on string leaves. Oversized trees pass
// through unchanged (responses are size-capped elsewhere; a cap must never
// turn into a silent partial redaction).
func redactAny(value any) any {
	walk := &redaction{}
	return walk.value(value, 0)
}

func (w *redaction) value(value any, depth int) any {
	if depth > redactMaxDepth || w.nodes > redactMaxNodes {
		return value
	}
	w.nodes++
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			if sensitiveKey(key) {
				out[key] = w.maskScalar(item)
				continue
			}
			out[key] = w.value(item, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = w.value(item, depth+1)
		}
		return out
	case string:
		masked := redactString(typed)
		if masked != typed {
			w.changes++
		}
		return masked
	default:
		return value
	}
}

// sensitiveKey reports whether a key name carries credentials or personal
// identifiers.
func sensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, fragment := range sensitiveKeyFragments {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

// maskScalar masks a value under a sensitive key without hiding its shape:
// strings collapse to a head…tail form, containers to their size — an agent
// still sees "a 214-char bearer token was here" without the token.
func (w *redaction) maskScalar(value any) any {
	w.changes++
	switch typed := value.(type) {
	case string:
		return maskString(typed)
	case map[string]any:
		return "<redacted object>"
	case []any:
		return "<redacted array>"
	default:
		return "***"
	}
}

// maskString keeps two edge runes so an agent can still tell two different
// tokens apart; short values collapse entirely.
func maskString(value string) string {
	runes := []rune(value)
	if len(runes) <= 6 {
		return "***"
	}
	return string(runes[:2]) + "…" + string(runes[len(runes)-2:])
}

// redactString masks identity-shaped substrings in free text (URLs, bodies,
// rendered command lines).
func redactString(value string) string {
	value = phonePattern.ReplaceAllString(value, "${1}<phone>${3}")
	return digitRunPattern.ReplaceAllStringFunc(value, func(run string) string {
		if idShapePattern.MatchString(run) {
			return "<id>"
		}
		return run
	})
}

// curlHeaderPattern matches one `-H 'Name: value'` (bash) or `-H "Name: value"`
// (cmd) token in a rendered curl line. Values with embedded quotes break the
// match and pass through unmasked — accepted, header values rarely carry them.
var curlHeaderPattern = regexp.MustCompile(`-H (?:'([^:]+): (.*?)'|"([^:]+): (.*?)")`)

// redactCurl masks the sensitive header values of a rendered curl line and
// runs the identity rules over the rest (URL query, --data-raw body).
func redactCurl(line string) string {
	line = curlHeaderPattern.ReplaceAllStringFunc(line, func(match string) string {
		parts := curlHeaderPattern.FindStringSubmatch(match)
		name := parts[1]
		value := parts[2]
		if name == "" {
			name, value = parts[3], parts[4]
		}
		if sensitiveKey(name) {
			w := &redaction{}
			return match[:len(match)-len(value)-1] + w.mask(value) + match[len(match)-1:]
		}
		return match
	})
	return redactString(line)
}

// mask applies the scalar mask to a plain string.
func (w *redaction) mask(value string) string {
	return maskString(value)
}

// redactBody masks one stored body for the reply: JSON bodies go through the
// deep walker, free text through the identity rules. Masking runs before any
// truncation so a cut can never split a mask. The bool says whether anything
// changed — the reply labels itself honestly.
func redactBody(body []byte) ([]byte, bool) {
	var decoded any
	if json.Unmarshal(body, &decoded) == nil {
		walk := &redaction{}
		masked := walk.value(decoded, 0)
		if out, err := json.Marshal(masked); err == nil {
			return out, walk.changes > 0
		}
		return body, false
	}
	text := string(body)
	masked := redactString(text)
	return []byte(masked), masked != text
}

// redactEnabled reads the per-call redact flag; masking is the default.
func redactEnabled(args map[string]any) bool {
	if value, ok := args["redact"].(bool); ok {
		return value
	}
	return true
}

// redactSensitiveValue masks one value that is sensitive by its key — for
// callers that know the storage key name (the reply's own field name is a
// generic "value" the key rules cannot see).
func redactSensitiveValue(value any) any {
	w := &redaction{}
	return w.maskScalar(value)
}

// stampRedacted labels a map-shaped reply with the redact decision.
func stampRedacted(result any, redacted bool) any {
	if m, ok := result.(map[string]any); ok {
		m["redacted"] = redacted
	}
	return result
}

// redactedPage is a DrainPage reply stamped redacted:true — every masked
// payload labels itself so an agent knows which replies carry raw values.
type redactedPage struct {
	DrainPage
	Redacted bool `json:"redacted"`
}
