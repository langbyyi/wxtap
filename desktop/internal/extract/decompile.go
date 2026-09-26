package extract

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var defineModulePattern = regexp.MustCompile(`(?:^|[;}\n])\s*define\s*\(\s*["']([^"']+)["']\s*,\s*function\s*\([^)]*\)\s*\{`)

// RestoreSourceTree restores developer-facing source files from the compiled
// files produced by WeChat. It deliberately leaves the unpacked originals in
// place so existing scanners and API consumers keep working.
func RestoreSourceTree(root string) ([]string, error) {
	var restored []string
	if files, err := restoreAppConfig(root); err != nil {
		return restored, err
	} else {
		restored = append(restored, files...)
	}
	if files, err := splitAppService(root); err != nil {
		return restored, err
	} else {
		restored = append(restored, files...)
	}
	if files, err := restoreWXML(root); err != nil {
		return restored, err
	} else {
		restored = append(restored, files...)
	}
	if files, err := restoreWebviewWXML(root); err != nil {
		return restored, err
	} else {
		restored = append(restored, files...)
	}
	if files, err := restoreWXSS(root); err != nil {
		return restored, err
	} else {
		restored = append(restored, files...)
	}
	if files, err := restoreWebviewWXSS(root); err != nil {
		return restored, err
	} else {
		restored = append(restored, files...)
	}
	return restored, nil
}

func restoreAppConfig(root string) ([]string, error) {
	source := filepath.Join(root, "app-config.json")
	data, err := os.ReadFile(source)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var config map[string]any
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse app-config.json: %w", err)
	}
	normalizeAppConfig(config)
	pretty, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("format app.json: %w", err)
	}
	pretty = append(pretty, '\n')
	target := filepath.Join(root, "app.json")
	if err := os.WriteFile(target, pretty, 0o600); err != nil {
		return nil, fmt.Errorf("write app.json: %w", err)
	}
	restored := []string{target}
	pageFiles, err := restorePageConfigs(root, config)
	if err != nil {
		return restored, err
	}
	return append(restored, pageFiles...), nil
}

func restorePageConfigs(root string, config map[string]any) ([]string, error) {
	pages, ok := config["page"].(map[string]any)
	if !ok {
		return nil, nil
	}
	var restored []string
	for pagePath, rawPage := range pages {
		page, ok := rawPage.(map[string]any)
		if !ok {
			continue
		}
		window, ok := page["window"].(map[string]any)
		if !ok || len(window) == 0 {
			continue
		}
		pretty, err := json.MarshalIndent(window, "", "  ")
		if err != nil {
			return restored, err
		}
		name := strings.TrimPrefix(trimPageExtension(strings.ReplaceAll(pagePath, "\\", "/")), "/") + ".json"
		target, err := safeChildPath(root, name)
		if err != nil {
			return restored, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return restored, err
		}
		if err := os.WriteFile(target, append(pretty, '\n'), 0o600); err != nil {
			return restored, err
		}
		restored = append(restored, target)
	}
	return restored, nil
}

func splitAppService(root string) ([]string, error) {
	sourcePath := filepath.Join(root, "app-service.js")
	data, err := os.ReadFile(sourcePath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	matches := defineModulePattern.FindAllStringSubmatchIndex(string(data), -1)
	written := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) < 4 {
			continue
		}
		name := string(data[match[2]:match[3]])
		openBrace := match[1] - 1
		closeBrace, ok := matchingDelimiter(string(data), openBrace, '{', '}')
		if !ok {
			return written, fmt.Errorf("unterminated module %s in app-service.js", name)
		}
		body := strings.TrimSpace(string(data[openBrace+1 : closeBrace]))
		target, err := safeChildPath(root, name)
		if err != nil {
			return written, fmt.Errorf("module %s: %w", name, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return written, err
		}
		if err := os.WriteFile(target, []byte(body+"\n"), 0o600); err != nil {
			return written, err
		}
		written = append(written, target)
	}
	pageConfigs, err := splitPageConfigs(root, data)
	if err != nil {
		return written, err
	}
	return append(written, pageConfigs...), nil
}

func matchingDelimiter(source string, start int, open, close byte) (int, bool) {
	if start < 0 || start >= len(source) || source[start] != open {
		return 0, false
	}
	depth := 0
	for i := start; i < len(source); i++ {
		switch source[i] {
		case '\'', '"':
			i = skipJavaScriptString(source, i)
		case '`':
			i = skipJavaScriptTemplate(source, i)
		case '/':
			if i+1 < len(source) && source[i+1] == '/' {
				i = skipJavaScriptLineComment(source, i+2)
			} else if i+1 < len(source) && source[i+1] == '*' {
				i = skipJavaScriptBlockComment(source, i+2)
			} else if isJavaScriptRegexStart(source, i) {
				i = skipJavaScriptRegex(source, i)
			}
		default:
			switch source[i] {
			case open:
				depth++
			case close:
				depth--
				if depth == 0 {
					return i, true
				}
			}
		}
	}
	return 0, false
}

func skipJavaScriptString(source string, start int) int {
	quote := source[start]
	for i := start + 1; i < len(source); i++ {
		if source[i] == '\\' {
			i++
			continue
		}
		if source[i] == quote {
			return i
		}
	}
	return len(source) - 1
}

// skipJavaScriptTemplate skips a template literal to its closing backtick,
// stepping over the ${...} substitutions it contains.
//
// Treating a template as a plain quoted string ends it at the first backtick
// inside it, and a substitution holding another template — `${cond?`a`:b}`,
// which compiled bundles produce freely — is then cut short. The scanner
// resumes in what is still template text, counts braces that are not code,
// and eventually lands inside a string where a URL's "//" swallows the rest of
// the line. A whole module can be lost that way.
func skipJavaScriptTemplate(source string, start int) int {
	for i := start + 1; i < len(source); i++ {
		switch source[i] {
		case '\\':
			i++
		case '`':
			return i
		case '$':
			// ${ opens a substitution whose braces are matched with the same
			// rules, so nested strings, regexes and templates inside it are
			// skipped rather than mistaken for text.
			if i+1 < len(source) && source[i+1] == '{' {
				closeBrace, ok := matchingDelimiter(source, i+1, '{', '}')
				if !ok {
					return len(source) - 1
				}
				i = closeBrace
			}
		}
	}
	return len(source) - 1
}

func skipJavaScriptLineComment(source string, start int) int {
	if index := strings.IndexByte(source[start:], '\n'); index >= 0 {
		return start + index
	}
	return len(source) - 1
}

func skipJavaScriptBlockComment(source string, start int) int {
	if index := strings.Index(source[start:], "*/"); index >= 0 {
		return start + index + 1
	}
	return len(source) - 1
}

func safeChildPath(root, name string) (string, error) {
	name = portableResourcePath(name)
	clean := filepath.Clean(filepath.FromSlash(strings.TrimLeft(name, `\/`)))
	if clean == "." || clean == "" || filepath.IsAbs(clean) {
		return "", fmt.Errorf("invalid relative path %q", name)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	target := filepath.Join(rootAbs, clean)
	rel, err := filepath.Rel(rootAbs, target)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes output directory: %q", name)
	}
	return target, nil
}

// portableResourcePath maps WeChat's virtual plugin URI to a regular output
// directory. Windows cannot create a filename containing the URI colon.
func portableResourcePath(name string) string {
	const pluginPrivate = "plugin-private://"
	if len(name) >= len(pluginPrivate) && strings.EqualFold(name[:len(pluginPrivate)], pluginPrivate) {
		return "plugin-private/" + name[len(pluginPrivate):]
	}
	return name
}

func normalizeAppConfig(config map[string]any) {
	if entry, ok := config["entryPagePath"].(string); ok {
		config["entryPagePath"] = trimPageExtension(entry)
	}
	if global, ok := config["global"].(map[string]any); ok {
		if window, ok := global["window"].(map[string]any); ok {
			config["window"] = window
		}
	}

	rawSubPackages, _ := config["subPackages"].([]any)
	if len(rawSubPackages) == 0 {
		rawSubPackages, _ = config["subpackages"].([]any)
	}
	roots := make([]string, 0, len(rawSubPackages))
	for _, item := range rawSubPackages {
		subpackage, ok := item.(map[string]any)
		if !ok {
			continue
		}
		root, _ := subpackage["root"].(string)
		root = strings.TrimPrefix(strings.ReplaceAll(root, "\\", "/"), "/")
		if root != "" && !strings.HasSuffix(root, "/") {
			root += "/"
		}
		subpackage["root"] = root
		if root != "" {
			roots = append(roots, root)
		}
		if pages, ok := subpackage["pages"].([]any); ok {
			for i, page := range pages {
				if name, ok := page.(string); ok {
					pages[i] = strings.TrimPrefix(trimPageExtension(name), root)
				}
			}
		}
	}
	if len(rawSubPackages) > 0 {
		config["subPackages"] = rawSubPackages
		delete(config, "subpackages")
	}

	rawPages, _ := config["pages"].([]any)
	mainPages := make([]any, 0, len(rawPages))
	for _, page := range rawPages {
		name, ok := page.(string)
		if !ok {
			continue
		}
		name = trimPageExtension(name)
		if isSubpackagePage(name, roots) {
			continue
		}
		mainPages = append(mainPages, name)
	}
	entry, _ := config["entryPagePath"].(string)
	if entry != "" {
		if !containsPage(mainPages, entry) {
			mainPages = append([]any{entry}, mainPages...)
		} else {
			mainPages = movePageFirst(mainPages, entry)
		}
	}
	config["pages"] = mainPages
}

func trimPageExtension(page string) string {
	lower := strings.ToLower(page)
	for _, extension := range []string{".wxml", ".html", ".js", ".json"} {
		if strings.HasSuffix(lower, extension) {
			return page[:len(page)-len(extension)]
		}
	}
	return page
}

func isSubpackagePage(page string, roots []string) bool {
	for _, root := range roots {
		if strings.HasPrefix(page, root) {
			return true
		}
	}
	return false
}

func containsPage(pages []any, page string) bool {
	for _, candidate := range pages {
		if candidate == page {
			return true
		}
	}
	return false
}

func movePageFirst(pages []any, page string) []any {
	reordered := make([]any, 0, len(pages))
	reordered = append(reordered, page)
	for _, candidate := range pages {
		if candidate != page {
			reordered = append(reordered, candidate)
		}
	}
	return reordered
}

func isJavaScriptRegexStart(source string, index int) bool {
	previous := index - 1
	for previous >= 0 && (source[previous] == ' ' || source[previous] == '\t' || source[previous] == '\r' || source[previous] == '\n') {
		previous--
	}
	if previous < 0 {
		return true
	}
	switch source[previous] {
	case '(', '[', '{', ':', ';', ',', '=', '!', '?', '&', '|', '+', '-', '*', '%', '^', '~', '<', '>':
		return true
	}
	end := previous + 1
	for previous >= 0 && isJavaScriptIdentifierByte(source[previous]) {
		previous--
	}
	switch source[previous+1 : end] {
	case "return", "throw", "case", "delete", "void", "typeof", "instanceof", "in", "of", "new", "yield", "await":
		return true
	default:
		return false
	}
}

func isJavaScriptIdentifierByte(value byte) bool {
	return value == '_' || value == '$' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func skipJavaScriptRegex(source string, start int) int {
	inClass := false
	for i := start + 1; i < len(source); i++ {
		switch source[i] {
		case '\\':
			i++
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '/':
			if !inClass {
				for i+1 < len(source) && isJavaScriptIdentifierByte(source[i+1]) {
					i++
				}
				return i
			}
		case '\n', '\r':
			return i - 1
		}
	}
	return len(source) - 1
}
