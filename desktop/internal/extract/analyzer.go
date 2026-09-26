package extract

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Analysis maps category keys (frontend contract) to the
// sorted, deduplicated values found in scanned content.
type Analysis map[string][]string

// Categories are all result keys, in display order.
var Categories = []string{
	"sfz", "mobile", "mail", "path", "url", "jwt", "secret", "key", "jdbc", "static", "oss",
}

type infoPattern struct {
	key  string
	re   *regexp.Regexp
	caps int // which capture group holds the value (0 = whole match)
}

// infoPatterns are the base PII/URL rules. The mail rule avoids RE2-
// unsupported negative lookahead; excluded extensions are filtered in
// mailExcludedExt afterwards.
var infoPatterns = []infoPattern{
	{"sfz", regexp.MustCompile(`['"]((\d{8}(0\d|10|11|12)([0-2]\d|30|31)\d{3}$)|(\d{6}(18|19|20)\d{2}(0[1-9]|10|11|12)([0-2]\d|30|31)\d{3}(\d|X|x)))['"]`), 1},
	{"mobile", regexp.MustCompile(`(?:^|[^\d])((?:\+?86)?1[3-9]\d{9})(?:$|[^\d])`), 1},
	{"mail", regexp.MustCompile(`['"]([a-zA-Z0-9._\-]*@[a-zA-Z0-9._\-]{1,63}\.[a-zA-Z]{2,})['"]`), 1},
	{"path", regexp.MustCompile(`(?:"|')(` +
		`(?:[a-zA-Z]{1,10}://|//)[^"'/]{1,}\.[a-zA-Z]{2,}[^"']{0,}` +
		`|(?:/|\.\./|\./)[^"'><,;|*()(%%$^/\\\[\]][^"'><,;|()]{1,}` +
		`|[a-zA-Z0-9_\-/]{1,}/[a-zA-Z0-9_\-/]{1,}\.(?:[a-zA-Z]{1,4}|action)(?:[\?|#][^"|']{0,}|)` +
		`|[a-zA-Z0-9_\-/]{1,}/[a-zA-Z0-9_\-/]{3,}(?:[\?|#][^"|']{0,}|)` +
		`|[a-zA-Z0-9_\-]{1,}\.(?:\w)(?:[\?|#][^"|']{0,}|)` +
		`)(?:"|')`), 1},
	{"jwt", regexp.MustCompile(`['"](ey[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{6,})['"]`), 1},
}

var jdbcPattern = regexp.MustCompile(`(?i)\b(?:jdbc:)?(?:mysql|postgresql|oracle|sqlserver|sqlite|mariadb)://[^\s'"]+`)

// mailExcludedExt filters file-extension lookalikes out of mail matches
// (RE2 has no negative lookahead, so extensions are filtered here).
var mailExcludedExt = map[string]bool{
	"js": true, "ts": true, "jsx": true, "tsx": true, "vue": true, "svelte": true,
	"css": true, "less": true, "scss": true, "sass": true, "map": true,
	"json": true, "xml": true, "html": true, "htm": true,
	"jpg": true, "jpeg": true, "png": true, "gif": true, "webp": true, "svg": true, "ico": true,
}

const dangerousExt = `php|asp|aspx|jsp|do|action|cgi|ashx|asmx|py|pl|go|rb|class|jar|war|ear|phar|` +
	`html|htm|exe|msi|bat|cmd|sh|bash|zsh|vbs|ps1|psm1|ps2|msh|msh1|msh2|mshxml|` +
	`msh1xml|msh2xml|dll|so|dylib|ocx|sys|drv|com|scr|pif|hta|vb|bas|frm|vbx|vbp|` +
	`service|desktop|application|command|tool|jspx|asa|cer|cdx|rhtml|tcl|tk|pm|rpm|` +
	`deb|apk|ipa|app|bin|elf|run|img|pkg|iso|dmg|nexe|node|wasm|clj|scala|groovy|` +
	`a|o|lib|ex_|inf|reg|lnk|scf`

const hostRe = `https?://[^\s'"/?#]+(?::\d+)?`

// urlPatterns: 4 cores × 2 quote styles (single and double quotes).
var urlPatterns = buildQuotedPatterns([]string{
	hostRe + `/*[^'"\s./?]+/*(?:\?[^\s'"]*)?`,
	hostRe + `/*[^'"\s/?]+\.(?:` + dangerousExt + `)/*(?:\?[^\s'"]*)?`,
	hostRe + `/*(?:/+[^'"\s/?]*)+/+[^'"\s./?]+/*(?:\?[^\s'"]*)?`,
	hostRe + `/*(?:/+[^'"\s/?]*)+/+[^'"\s/?]+\.(?:` + dangerousExt + `)/*(?:\?[^\s'"]*)?`,
})

const staticExt = `css|js|ts|json|config|xml|vue|jsx|tsx|txt|csv|` +
	`png|jpg|jpeg|gif|webp|svg|ico|bmp|` +
	`mp3|wav|ogg|mp4|webm|mov|avi|mkv|` +
	`pdf|doc|docx|xls|xlsx|ppt|pptx|rtf|` +
	`woff|woff2|ttf|eot|svelte|` +
	`zip|rar|7z|tar|gz|bz2|xz|tgz|tbz|lz|lz4|zst|iso`

var staticPatterns = buildQuotedPatterns([]string{
	`/+[^'"\s/?]+\.(?:` + staticExt + `)/*(?:\?[^\s'"]*)?`,
	`(?:/+[^'"\s/?]*)+/+[^'"\s/?]+\.(?:` + staticExt + `)/*(?:\?[^\s'"]*)?`,
	`https?://[^\s'"/?#]+(?::\d+)?/*[^'"\s/?]+\.(?:` + staticExt + `)/*(?:\?[^\s'"]*)?`,
	`https?://[^\s'"/?#]+(?::\d+)?/*(?:/+[^'"\s/?]*)+/+[^'"\s/?]+\.(?:` + staticExt + `)/*(?:\?[^\s'"]*)?`,
})

func buildQuotedPatterns(cores []string) []*regexp.Regexp {
	var pats []*regexp.Regexp
	for _, core := range cores {
		pats = append(pats,
			regexp.MustCompile(`(?im)(?:^|[^\\])["'](`+core+`)["']`),
			regexp.MustCompile(`\\["'](`+core+`)\\["']`),
		)
	}
	return pats
}

// staticFilterExts are source-map extensions excluded from static results.
var staticFilterExts = []string{
	".ts", ".tsx", ".less", ".scss", ".sass", ".map",
	".d.ts", ".spec.ts", ".test.ts",
}

var ossDomainPattern = regexp.MustCompile(`(?i)(?:` +
	`[\w.-]+\.oss[\w-]*\.aliyuncs\.com|` +
	`[\w.-]+\.cos\.[\w-]+\.myqcloud\.com|` +
	`[\w.-]+\.file\.myqcloud\.com|` +
	`(?:[\w.-]+\.)?s3[\w.-]*\.amazonaws\.com|` +
	`[\w.-]+\.obs\.[\w-]+\.myhuaweicloud\.com|` +
	`[\w.-]+\.(?:qiniucdn|qnssl)\.com|` +
	`[\w.-]+\.bkt\.clouddn\.com|` +
	`[\w.-]+\.blob\.core\.windows\.net|` +
	`[\w.-]+\.azureedge\.net|` +
	`storage\.googleapis\.com(?:/[\w.-]+)?|` +
	`[\w.-]+\.storage\.googleapis\.com|` +
	`[\w.-]+\.cdn\.bcebos\.com|` +
	`[\w.-]+\.vod[\w-]*\.aliyuncs\.com|` +
	`[\w.-]+\.cdn\.aliyuncs\.com|` +
	`[\w.-]+\.ucloud\.cn|` +
	`[\w.-]+\.ks3[\w-]*\.ksyun\.com)`)

// ossMarkers is a cheap prefilter for the cloud-storage alternatives. The
// full alternation is kept only for bounded candidate tokens; running it over
// a multi-megabyte file can make the regexp engine explore too many states.
var ossMarkers = []string{
	"oss", "myqcloud", "amazonaws", "myhuaweicloud", "qiniucdn", "qnssl",
	"clouddn", "core.windows.net", "azureedge.net", "storage.googleapis.com",
	"bcebos", "ucloud", "ksyun",
}

func findOSSDomains(content string) []string {
	lower := strings.ToLower(content)
	found := false
	for _, marker := range ossMarkers {
		if strings.Contains(lower, marker) {
			found = true
			break
		}
	}
	if !found {
		return nil
	}

	// Anchor the expensive alternation to small windows around known cloud
	// markers. This keeps minified one-line bundles searchable without
	// exposing the whole 2 MB input to the alternation at every position.
	const markerWindow = 1024
	var matches []string
	for _, marker := range ossMarkers {
		offset := 0
		for {
			index := strings.Index(lower[offset:], marker)
			if index < 0 {
				break
			}
			index += offset
			start := index - markerWindow
			if start < 0 {
				start = 0
			}
			end := index + len(marker) + markerWindow
			if end > len(content) {
				end = len(content)
			}
			matches = append(matches, ossDomainPattern.FindAllString(content[start:end], -1)...)
			offset = index + len(marker)
		}
	}
	return matches
}

// nucleiKeyPattern matches the generated credential key-name rules. It is
// anchored and evaluated only against the bounded key token extracted by
// nucleiAssignmentPattern, avoiding a 700-way alternation over every byte of
// a multi-megabyte bundle.
var nucleiKeyPattern = regexp.MustCompile(`(?i)^["']?(?:` + strings.Join(nucleiKeyNames, "|") + `)["']?$`)

// nucleiAssignmentPattern finds `key = value` / `key: value` candidates before
// the credential-key filter. Keep the key and value bounded: a malformed or
// minified file must not turn a scan into an unbounded regexp input.
var nucleiAssignmentPattern = regexp.MustCompile(`(?im)["']?[\w.-]{1,100}["']?[^\S\r\n]*[=:][^\S\r\n]*["']?[\w./+=\-]{1,500}["']?`)

func credentialAssignments(content string) []string {
	matches := nucleiAssignmentPattern.FindAllString(content, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		separator := strings.IndexAny(match, "=:")
		if separator < 0 {
			continue
		}
		key := strings.TrimSpace(match[:separator])
		key = strings.Trim(key, `"'`)
		if nucleiKeyPattern.MatchString(key) {
			out = append(out, match)
		}
	}
	return out
}

// specialSecretPatterns match fixed-format cloud/API credential strings.
var specialSecretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)["']?[-]+BEGIN \w+ PRIVATE KEY[-]+`),
	regexp.MustCompile(`LTAI[A-Za-z\d]{12,30}`),
	regexp.MustCompile(`AKID[A-Za-z\d]{13,40}`),
	regexp.MustCompile(`JDC_[0-9A-Z]{25,40}`),
	regexp.MustCompile(`(?:A3T[A-Z0-9]|AKIA|AGPA|AIDA|AROA|AIPA|ANPA|ANVA|ASIA)[A-Z0-9]{16}`),
	regexp.MustCompile(`(?:AKLT|AKTP)[a-zA-Z0-9]{35,50}`),
	regexp.MustCompile(`AKLT[a-zA-Z0-9\-_]{16,28}`),
	regexp.MustCompile(`AIza[0-9A-Za-z_\-]{35}`),
	regexp.MustCompile(`[Bb]earer\s+[a-zA-Z0-9\-=._+/\\]{20,500}`),
	regexp.MustCompile(`[Bb]asic\s+[A-Za-z0-9+/]{18,}={0,2}`),
	regexp.MustCompile(`["'\[]*[Aa]uthorization["'\]]*\s*[:=]\s*['"]?\b(?:[Tt]oken\s+)?[a-zA-Z0-9\-_+/]{20,500}['"]?`),
	regexp.MustCompile(`glpat-[a-zA-Z0-9\-=_]{20,22}`),
	regexp.MustCompile(`(?:ghp|gho|ghu|ghs|ghr|github_pat)_[a-zA-Z0-9_]{36,255}`),
	regexp.MustCompile(`APID[a-zA-Z0-9]{32,42}`),
	regexp.MustCompile(`["'](wx[a-z0-9]{15,18})["']`),
	regexp.MustCompile(`["'](ww[a-z0-9]{15,18})["']`),
	regexp.MustCompile(`["'](gh_[a-z0-9]{11,13})["']`),
	regexp.MustCompile(`(?i)(?:admin_?pass|password|[a-z]{3,15}_?password|user_?pass|user_?pwd|admin_?pwd)\\?['"]*\s*[:=]\s*\\?['"][a-z0-9!@#$%&*]{5,20}\\?['"]`),
	regexp.MustCompile(`(?i)https://qyapi\.weixin\.qq\.com/cgi-bin/webhook/send\?key=[a-zA-Z0-9\-]{25,50}`),
	regexp.MustCompile(`(?i)https://oapi\.dingtalk\.com/robot/send\?access_token=[a-z0-9]{50,80}`),
	regexp.MustCompile(`(?i)https://open\.feishu\.cn/open-apis/bot/v2/hook/[a-z0-9\-]{25,50}`),
	regexp.MustCompile(`(?i)https://hooks\.slack\.com/services/[a-zA-Z0-9\-_]{6,12}/[a-zA-Z0-9\-_]{6,12}/[a-zA-Z0-9\-_]{15,24}`),
	regexp.MustCompile(`eyJrIjoi[a-zA-Z0-9\-_+/]{50,100}={0,2}`),
	regexp.MustCompile(`glc_[A-Za-z0-9\-_+/]{32,200}={0,2}`),
	regexp.MustCompile(`glsa_[A-Za-z0-9]{32}_[A-Fa-f0-9]{8}`),
}

const maxAnalyzeLen = 2_000_000

// Analyze extracts all sensitive-information categories from one file's content.
func Analyze(content string) Analysis {
	if len(content) > maxAnalyzeLen {
		content = content[:maxAnalyzeLen]
	}
	result := Analysis{}
	for _, key := range Categories {
		result[key] = nil
	}

	// 1) Base information patterns.
	for _, p := range infoPatterns {
		seen := map[string]bool{}
		for _, m := range p.re.FindAllStringSubmatch(content, -1) {
			value := stripQuotes(m[p.caps])
			if value == "" || seen[value] {
				continue
			}
			if p.key == "sfz" && !isValidChineseID(value) {
				continue
			}
			if p.key == "mail" && mailExcludedExt[strings.ToLower(extensionOf(value))] {
				continue
			}
			seen[value] = true
			result[p.key] = append(result[p.key], value)
		}
	}
	// 2) URL extraction.
	urlSet := map[string]bool{}
	for _, p := range urlPatterns {
		for _, m := range p.FindAllStringSubmatch(content, -1) {
			value := strings.TrimSpace(m[1])
			if isHTTPURL(value) {
				urlSet[value] = true
			}
		}
	}

	// 3) Static asset extraction, filtering source-map extensions.
	staticSet := map[string]bool{}
	for _, p := range staticPatterns {
		for _, m := range p.FindAllStringSubmatch(content, -1) {
			v := strings.TrimSpace(m[1])
			if !hasAnySuffix(v, staticFilterExts) {
				staticSet[v] = true
			}
		}
	}

	// 4) Remove static assets from the URL set.
	for v := range staticSet {
		delete(urlSet, v)
	}

	// 5) Path post-processing: drop noise.
	filteredPath := result["path"][:0:0]
	for _, p := range result["path"] {
		if !strings.Contains(p, "/") || staticSet[p] || urlSet[p] || hasAnySuffix(p, staticFilterExts) ||
			strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") || strings.HasPrefix(p, "//") {
			continue
		}
		filteredPath = append(filteredPath, p)
	}
	result["path"] = filteredPath

	result["url"] = sortedKeys(urlSet)
	result["static"] = sortedKeys(staticSet)

	// 7) Secrets: credential key-name rules + fixed-format rules.
	secretSet := map[string]bool{}
	keySet := map[string]bool{}
	for _, m := range credentialAssignments(content) {
		value := strings.TrimSpace(m)
		if isPlaceholderValue("secret", value) {
			continue
		}
		secretSet[value] = true
		keySet[value] = true
	}
	for _, p := range specialSecretPatterns {
		for _, m := range p.FindAllString(content, -1) {
			secretSet[strings.TrimSpace(m)] = true
		}
	}
	result["secret"] = sortedKeys(secretSet)
	result["key"] = sortedKeys(keySet)
	jdbcSet := map[string]bool{}
	for _, m := range jdbcPattern.FindAllString(content, -1) {
		jdbcSet[strings.TrimSpace(m)] = true
	}
	result["jdbc"] = sortedKeys(jdbcSet)

	// 8) OSS cloud storage detection across URL-ish results and raw content.
	ossSet := map[string]bool{}
	for _, item := range result["url"] {
		if ossDomainPattern.MatchString(item) {
			ossSet[item] = true
		}
	}
	for _, item := range result["static"] {
		if ossDomainPattern.MatchString(item) {
			ossSet[item] = true
		}
	}
	for _, m := range findOSSDomains(content) {
		ossSet[m] = true
	}
	result["oss"] = sortedKeys(ossSet)

	for _, key := range Categories {
		sort.Strings(result[key])
	}
	return result
}

// Merge combines per-file analyses into one deduplicated result.
func Merge(results []Analysis) Analysis {
	merged := Analysis{}
	for _, key := range Categories {
		merged[key] = nil
	}
	seen := map[string]map[string]bool{}
	for _, key := range Categories {
		seen[key] = map[string]bool{}
	}
	for _, r := range results {
		for _, key := range Categories {
			for _, v := range r[key] {
				if !seen[key][v] {
					seen[key][v] = true
					merged[key] = append(merged[key], v)
				}
			}
		}
	}
	for _, key := range Categories {
		sort.Strings(merged[key])
	}
	return merged
}

func stripQuotes(s string) string {
	if len(s) >= 2 {
		if s[0] == '\'' || s[0] == '"' {
			s = s[1:]
		}
		if len(s) > 0 && (s[len(s)-1] == '\'' || s[len(s)-1] == '"') {
			s = s[:len(s)-1]
		}
	}
	return s
}

func isHTTPURL(value string) bool {
	parsed, err := url.ParseRequestURI(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func isValidChineseID(value string) bool {
	if len(value) != 18 {
		return false
	}
	if _, err := time.Parse("20060102", value[6:14]); err != nil {
		return false
	}
	weights := [...]int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}
	checks := "10X98765432"
	sum := 0
	for index, weight := range weights {
		digit := value[index]
		if digit < '0' || digit > '9' {
			return false
		}
		sum += int(digit-'0') * weight
	}
	return strings.ToUpper(value[17:]) == string(checks[sum%11])
}

func extensionOf(value string) string {
	if i := strings.LastIndex(value, "."); i >= 0 {
		return value[i+1:]
	}
	return ""
}

func hasAnySuffix(s string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// PatternStrings exposes one representative regex string per analyzer
// category for the UI's read-only builtin-pattern list (port of
// Extractor.get_all_builtin_patterns).
func PatternStrings() map[string]string {
	out := map[string]string{}
	for _, p := range infoPatterns {
		out[p.key] = p.re.String()
	}
	out["key"] = nucleiAssignmentPattern.String()
	out["jdbc"] = jdbcPattern.String()
	out["url"] = urlPatterns[0].String()
	out["static"] = staticPatterns[0].String()
	out["oss"] = ossDomainPattern.String()
	out["secret"] = specialSecretPatterns[0].String()
	return out
}
