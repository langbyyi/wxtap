package extract

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dop251/goja"
)

var wxssCallPattern = regexp.MustCompile(`(?s)setCssToHead\s*\(`)

func restoreWXSS(root string) ([]string, error) {
	sourcePath, data, err := readStyleSource(root)
	if err != nil || sourcePath == "" {
		return nil, err
	}
	if len(data) > maxRestoreScriptBytes {
		return nil, fmt.Errorf("style bundle is too large to restore safely: %d bytes", len(data))
	}
	if strings.HasSuffix(strings.ToLower(sourcePath), ".html") {
		matches := scriptBlockPattern.FindAllSubmatch(data, -1)
		var scripts []byte
		for _, match := range matches {
			if len(match) > 1 {
				scripts = append(scripts, match[1]...)
				scripts = append(scripts, '\n')
			}
		}
		data = scripts
	}
	if !wxssCallPattern.Match(data) {
		return nil, nil
	}

	styles := map[string]string{}
	vm := goja.New()
	timer := time.AfterFunc(wxmlRestoreTimeout, func() {
		vm.Interrupt("WXSS restoration timed out")
	})
	defer timer.Stop()

	callback := func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) < 2 {
			return goja.Undefined()
		}
		stylePath := stylePathFromCall(call)
		if stylePath == "" {
			return goja.Undefined()
		}
		styles[stylePath] += styleTokensToCSS(call.Argument(0).Export(), stylePath)
		return goja.Undefined()
	}
	if err := vm.Set("setCssToHead", callback); err != nil {
		return nil, err
	}
	patch := `
var noCss = true;
var window = {};
var navigator = {userAgent: "iPhone"};
var document = {getElementsByTagName: function() { return []; }};
var define = function() {};
var require = function() {};
var __COMMON_STYLESHEETS__ = {};
`
	if _, err := vm.RunString(patch + "\ntry {\n" + string(data) + "\n} catch (e) {}\n"); err != nil {
		return nil, fmt.Errorf("initialize WXSS runtime: %w", err)
	}

	var written []string
	for stylePath, style := range styles {
		if strings.TrimSpace(style) == "" {
			continue
		}
		target, err := safeChildPath(root, stylePath)
		if err != nil {
			return written, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return written, err
		}
		if err := os.WriteFile(target, []byte(style), 0o600); err != nil {
			return written, err
		}
		written = append(written, target)
	}
	return written, nil
}

func readStyleSource(root string) (string, []byte, error) {
	for _, name := range []string{"app-wxss.js", "page-frame.js", "pageframe.js", "page-frame.html"} {
		file := filepath.Join(root, name)
		data, err := os.ReadFile(file)
		if err == nil {
			return file, data, nil
		}
		if !os.IsNotExist(err) {
			return "", nil, err
		}
	}
	return "", nil, nil
}

func stylePathFromCall(call goja.FunctionCall) string {
	for i := 1; i < len(call.Arguments); i++ {
		if metadata, ok := call.Argument(i).Export().(map[string]any); ok {
			if value, ok := metadata["path"].(string); ok && value != "" {
				return value
			}
		}
	}
	if len(call.Arguments) >= 2 {
		if value, ok := call.Argument(1).Export().(string); ok && value != "" {
			return value
		}
	}
	return ""
}

func styleTokensToCSS(value any, stylePath string) string {
	switch token := value.(type) {
	case string:
		return token
	case []any:
		if len(token) == 1 {
			if number, ok := styleNumber(token[0]); ok && number == 1 {
				return ""
			}
		}
		if len(token) >= 2 {
			kind, ok := styleNumber(token[0])
			if ok {
				switch kind {
				case 0:
					if number, numberOK := styleNumber(token[1]); numberOK {
						return formatStyleNumber(number) + "rpx"
					}
				case 2:
					switch target := token[1].(type) {
					case string:
						if target != "" {
							return fmt.Sprintf("@import %q;\n", relativeStylePath(stylePath, target))
						}
					case []any:
						return styleTokensToCSS(target, stylePath)
					}
				}
			}
		}
		var builder strings.Builder
		for _, item := range token {
			builder.WriteString(styleTokensToCSS(item, stylePath))
		}
		return builder.String()
	case map[string]any:
		data, err := json.Marshal(token)
		if err == nil {
			return string(data)
		}
		return fmt.Sprint(token)
	case nil:
		return ""
	default:
		return fmt.Sprint(token)
	}
}

func styleNumber(value any) (int64, bool) {
	switch number := value.(type) {
	case int64:
		return number, true
	case int:
		return int64(number), true
	case float64:
		if math.Trunc(number) == number {
			return int64(number), true
		}
	case json.Number:
		value, err := number.Int64()
		return value, err == nil
	}
	return 0, false
}

func formatStyleNumber(value int64) string {
	return strconv.FormatInt(value, 10)
}

func relativeStylePath(stylePath, target string) string {
	dir := path.Dir(strings.ReplaceAll(stylePath, "\\", "/"))
	target = strings.ReplaceAll(target, "\\", "/")
	target = strings.TrimPrefix(target, "/")
	relative, err := filepath.Rel(filepath.FromSlash(dir), filepath.FromSlash(target))
	if err != nil {
		return target
	}
	return filepath.ToSlash(relative)
}

var commonStylePattern = regexp.MustCompile(`__COMMON_STYLESHEETS__\s*\[\s*["']([^"']+\.wxss)["']\s*\]\s*=\s*\[`)

func restoreWebviewWXSS(root string) ([]string, error) {
	chunks, err := filepath.Glob(filepath.Join(root, "*.webview.js"))
	if err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, nil
	}

	styles := map[string]string{}
	common := map[string]any{}
	vm := goja.New()
	if err := vm.Set("__COMMON_STYLESHEETS__", &common); err != nil {
		return nil, err
	}
	callback := func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) < 2 {
			return goja.Undefined()
		}
		stylePath := stylePathFromCall(call)
		if stylePath == "" {
			return goja.Undefined()
		}
		styles[stylePath] += styleTokensToCSS(resolveStyleTokens(call.Argument(0).Export(), common), stylePath)
		return goja.Undefined()
	}
	if err := vm.Set("setCssToHead", callback); err != nil {
		return nil, err
	}

	if runtimePath := filepath.Join(root, "app-wxss.js"); fileExists(runtimePath) {
		data, err := os.ReadFile(runtimePath)
		if err == nil && len(data) <= maxRestoreScriptBytes {
			for _, match := range commonStylePattern.FindAllSubmatchIndex(data, -1) {
				if len(match) < 4 {
					continue
				}
				name := string(data[match[2]:match[3]])
				openBracket := match[1] - 1
				closeBracket, ok := matchingDelimiter(string(data), openBracket, '[', ']')
				if !ok {
					continue
				}
				expression := string(data[match[0]:closeBracket+1]) + ";"
				if _, err := vm.RunString(expression); err != nil {
					continue
				}
				value, ok := common[name]
				if !ok {
					continue
				}
				styles[name] += styleTokensToCSS(value, name)
			}
		}
	}

	for _, chunkPath := range chunks {
		data, err := os.ReadFile(chunkPath)
		if err != nil || len(data) > maxRestoreScriptBytes {
			continue
		}
		for _, start := range wxssCallPattern.FindAllIndex(data, -1) {
			openParen := start[1] - 1
			closeParen, ok := matchingDelimiter(string(data), openParen, '(', ')')
			if !ok {
				continue
			}
			expression := string(data[start[0]:closeParen+1]) + ";"
			if _, err := vm.RunString(expression); err != nil {
				continue
			}
		}
	}

	var written []string
	for stylePath, style := range styles {
		if strings.TrimSpace(style) == "" {
			continue
		}
		target, err := safeChildPath(root, stylePath)
		if err != nil {
			return written, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return written, err
		}
		if err := os.WriteFile(target, []byte(style), 0o600); err != nil {
			return written, err
		}
		written = append(written, target)
	}
	return written, nil
}

func resolveStyleTokens(value any, common map[string]any) any {
	if name, ok := value.(string); ok {
		if registered, exists := common[name]; exists {
			return registered
		}
	}
	return value
}
