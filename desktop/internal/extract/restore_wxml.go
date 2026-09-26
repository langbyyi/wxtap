package extract

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dop251/goja"
)

const (
	maxRestoreScriptBytes = 32 << 20
	wxmlRestoreTimeout    = 3 * time.Second
)

var (
	wxmlAssignmentPattern = regexp.MustCompile(`__wxAppCode__\s*\[\s*["']([^"']+\.wxml)["']\s*\]\s*=\s*([A-Za-z_$][\w$]*\s*\(\s*["'][^"']+["']\s*\)\s*;)`)
	scriptBlockPattern    = regexp.MustCompile(`(?is)<script\b[^>]*>(.*?)</script>`)
	// Decompiled page frames can retain an else branch whose preceding if was
	// stripped. Execute the assignment directly instead of feeding invalid
	// JavaScript to goja.
	looseWXMLElsePattern = regexp.MustCompile(`(?m)\belse\s+(__wxAppCode__\s*\[)`)
)

func restoreWXML(root string) ([]string, error) {
	framePath, data, err := readFrameSource(root)
	if err != nil || framePath == "" {
		return nil, err
	}
	if len(data) > maxRestoreScriptBytes {
		return nil, fmt.Errorf("page frame is too large to restore safely: %d bytes", len(data))
	}
	if strings.HasSuffix(strings.ToLower(framePath), ".html") {
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

	data = looseWXMLElsePattern.ReplaceAll(data, []byte("$1"))
	assignments := wxmlAssignmentPattern.FindAllSubmatch(data, -1)
	if len(assignments) == 0 {
		return nil, nil
	}
	// Template factories and their registration statements are not always emitted
	// in dependency order. Load only the factories first, then evaluate each
	// registration below after every factory has been declared.
	runtimeData := wxmlAssignmentPattern.ReplaceAll(data, nil)
	vm := goja.New()
	timer := time.AfterFunc(wxmlRestoreTimeout, func() {
		vm.Interrupt("WXML restoration timed out")
	})
	defer timer.Stop()

	patch := `
var noCss = true;
var console = {log:function(){},warn:function(){},error:function(){}};
var window = {screen: {}};
var navigator = {userAgent: "iPhone"};
var document = {getElementsByTagName: function() { return []; }};
var define = function() {};
var require = function() {};
var setCssToHead = function() {};
var __wxAppCode__ = {};
var __wxConfig = {};
var __vd_version_info__ = {};
`
	if _, err := vm.RunString(patch + "\ntry {\n" + string(runtimeData) + "\n} catch (e) {}\n"); err != nil {
		return nil, fmt.Errorf("initialize WXML runtime: %w", err)
	}

	var written []string
	for _, assignment := range assignments {
		if len(assignment) < 3 {
			continue
		}
		name := string(assignment[1])
		expression := string(assignment[2])
		value, err := vm.RunString(expression)
		if err != nil {
			// A generated template can depend on browser-only runtime state. Its
			// raw source was already extracted, so leave that one template intact
			// instead of failing the whole mini-program decompilation.
			continue
		}
		value, err = resolveFunctionResult(vm, value)
		if err != nil {
			continue
		}
		rendered, err := renderWXMLValue(value.Export())
		if err != nil {
			continue
		}
		if strings.TrimSpace(rendered) == "" {
			continue
		}
		target, err := safeChildPath(root, name)
		if err != nil {
			return written, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return written, err
		}
		if err := os.WriteFile(target, []byte(strings.TrimRight(rendered, "\n")+"\n"), 0o600); err != nil {
			return written, err
		}
		written = append(written, target)
	}
	return written, nil
}

func readFrameSource(root string) (string, []byte, error) {
	for _, name := range []string{"page-frame.html", "page-frame.js", "pageframe.js"} {
		path := filepath.Join(root, name)
		data, err := os.ReadFile(path)
		if err == nil {
			return path, data, nil
		}
		if !os.IsNotExist(err) {
			return "", nil, err
		}
	}
	return "", nil, nil
}

func resolveFunctionResult(vm *goja.Runtime, value goja.Value) (goja.Value, error) {
	if value == nil || goja.IsNull(value) || goja.IsUndefined(value) {
		return value, nil
	}
	for depth := 0; depth < 32 && value.ExportType() != nil && value.ExportType().Kind() == reflect.Func; depth++ {
		fn, ok := goja.AssertFunction(value)
		if !ok {
			return nil, fmt.Errorf("expected render function, got %T", value.Export())
		}
		next, err := fn(goja.Undefined())
		if err != nil {
			return nil, err
		}
		value = next
	}
	if value.ExportType() != nil && value.ExportType().Kind() == reflect.Func {
		return nil, fmt.Errorf("render function recursion limit exceeded")
	}
	return value, nil
}

func renderWXMLValue(value any) (string, error) {
	normalized, err := normalizeExport(value)
	if err != nil {
		return "", err
	}
	return renderWXMLNode(normalized, 0), nil
}

func normalizeExport(value any) (any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		// A value that still holds functions where a node tree belongs is the
		// compiled-template signature, and the only failure of this step that
		// says anything about the templates. It is recognised by looking at the
		// value, never by matching encoding/json's wording.
		if exportHasFuncs(value) {
			return nil, &funcExportError{cause: err}
		}
		return nil, err
	}
	var normalized any
	if err := json.Unmarshal(data, &normalized); err != nil {
		return nil, err
	}
	return normalized, nil
}

// funcExportError marks the one webview result that cannot be rendered by any
// amount of retrying: the chunk handed out functions where data belongs.
type funcExportError struct{ cause error }

func (e *funcExportError) Error() string { return e.cause.Error() }

func (e *funcExportError) Unwrap() error { return e.cause }

// isFuncExport reports whether a step failure is that signature.
func isFuncExport(err error) bool {
	var target *funcExportError
	return errors.As(err, &target)
}

// exportHasFuncs reports whether a value from a webview chunk still contains
// functions anywhere in its tree. The walk is bounded and cycle-safe: the value
// comes from a script this restorer does not control, and it may hold pointers
// or reach the depth limit.
func exportHasFuncs(value any) bool {
	return valueHasFuncs(reflect.ValueOf(value), 0, map[uintptr]bool{})
}

func valueHasFuncs(value reflect.Value, depth int, seen map[uintptr]bool) bool {
	if depth > 64 || !value.IsValid() {
		return false
	}
	switch value.Kind() {
	case reflect.Func:
		return true
	case reflect.Interface:
		return valueHasFuncs(value.Elem(), depth+1, seen)
	case reflect.Pointer:
		if value.IsNil() {
			return false
		}
		pointer := value.Pointer()
		if seen[pointer] {
			return false
		}
		seen[pointer] = true
		return valueHasFuncs(value.Elem(), depth+1, seen)
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			if valueHasFuncs(value.Index(i), depth+1, seen) {
				return true
			}
		}
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			if valueHasFuncs(iter.Value(), depth+1, seen) {
				return true
			}
		}
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			if valueHasFuncs(value.Field(i), depth+1, seen) {
				return true
			}
		}
	}
	return false
}

func renderWXMLNode(node any, depth int) string {
	switch value := node.(type) {
	case []any:
		var parts []string
		for _, child := range value {
			if rendered := renderWXMLNode(child, depth); rendered != "" {
				parts = append(parts, rendered)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		children := value["children"]
		tag, _ := value["tag"].(string)
		if tag == "" {
			return renderWXMLNode(children, depth)
		}
		tag = strings.TrimPrefix(tag, "wx-")
		if tag == "page" {
			return renderWXMLNode(children, depth)
		}
		indent := strings.Repeat("  ", depth)
		var builder strings.Builder
		builder.WriteString(indent)
		builder.WriteByte('<')
		builder.WriteString(tag)
		if attrs, ok := value["attr"].(map[string]any); ok {
			keys := make([]string, 0, len(attrs))
			for key := range attrs {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				name := strings.TrimPrefix(key, "$wxs:")
				if strings.HasPrefix(name, "$") {
					continue
				}
				builder.WriteByte(' ')
				builder.WriteString(name)
				if attrs[key] != nil {
					builder.WriteString(`="`)
					builder.WriteString(html.EscapeString(formatWXMLAttribute(attrs[key])))
					builder.WriteByte('"')
				}
			}
		}
		builder.WriteByte('>')
		if text, ok := singleWXMLText(children); ok {
			builder.WriteString(html.EscapeString(strings.TrimSpace(text)))
		} else if children != nil {
			builder.WriteByte('\n')
			switch childValue := children.(type) {
			case []any:
				for _, child := range childValue {
					rendered := renderWXMLNode(child, depth+1)
					if rendered != "" {
						builder.WriteString(rendered)
						builder.WriteByte('\n')
					}
				}
			default:
				rendered := renderWXMLNode(childValue, depth+1)
				if rendered != "" {
					builder.WriteString(rendered)
					builder.WriteByte('\n')
				}
			}
			builder.WriteString(indent)
		}
		builder.WriteString("</")
		builder.WriteString(tag)
		builder.WriteByte('>')
		return builder.String()
	default:
		return fmt.Sprint(value)
	}
}

func singleWXMLText(value any) (string, bool) {
	switch value := value.(type) {
	case string:
		return value, strings.TrimSpace(value) != ""
	case []any:
		if len(value) == 1 {
			text, ok := value[0].(string)
			return text, ok && strings.TrimSpace(text) != ""
		}
	}
	return "", false
}

func formatWXMLAttribute(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case []any, map[string]any:
		data, err := json.Marshal(value)
		if err == nil {
			return string(data)
		}
	}
	return fmt.Sprint(value)
}

func restoreWebviewWXML(root string) ([]string, error) {
	chunks, err := filepath.Glob(filepath.Join(root, "*.webview.js"))
	if err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, nil
	}

	vm := newRestoreRuntime()
	timer := time.AfterFunc(wxmlRestoreTimeout, func() {
		vm.Interrupt("WXML chunk restoration timed out")
	})
	defer timer.Stop()

	if runtimePath := filepath.Join(root, "app-wxss.js"); fileExists(runtimePath) {
		if data, err := os.ReadFile(runtimePath); err == nil && len(data) <= maxRestoreScriptBytes {
			_, _ = vm.RunString(wrapRestoreScript(string(data)))
		}
	}
	for _, chunkPath := range chunks {
		data, err := os.ReadFile(chunkPath)
		if err != nil || len(data) > maxRestoreScriptBytes {
			continue
		}
		_, _ = vm.RunString(wrapRestoreScript(string(data)))
	}

	appCodeValue := vm.Get("__wxAppCode__")
	if appCodeValue == nil || goja.IsUndefined(appCodeValue) || goja.IsNull(appCodeValue) {
		return nil, nil
	}
	appCode := appCodeValue.ToObject(vm)
	if appCode == nil {
		return nil, nil
	}

	var written []string
	var firstErr error
	sawFuncExport := false
	for _, name := range appCode.Keys() {
		if !strings.HasSuffix(name, ".wxml") {
			continue
		}
		value, err := resolveWebviewResult(vm, appCode.Get(name))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		rendered, err := renderWXMLValue(value.Export())
		if err != nil {
			if isFuncExport(err) {
				sawFuncExport = true
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if strings.TrimSpace(rendered) == "" {
			continue
		}
		target, err := safeChildPath(root, name)
		if err != nil {
			return written, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return written, err
		}
		if err := os.WriteFile(target, []byte(strings.TrimRight(rendered, "\n")+"\n"), 0o600); err != nil {
			return written, err
		}
		written = append(written, target)
	}
	// A verdict needs the signature, not just an empty result: a timeout or a
	// template that depends on browser-only state fails this same step while
	// saying nothing about the templates.
	if len(written) == 0 && sawFuncExport {
		return nil, &unrenderableTemplatesError{cause: explainWebviewFailure(firstErr)}
	}
	if len(written) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return written, nil
}

// unrenderableTemplatesError is the one webview failure that is a verdict: the
// chunks handed out templates this restorer cannot render, and nothing was
// written. Callers may only hide an app on this error — the step also fails on a
// timeout or an I/O error, and neither says anything about the templates.
type unrenderableTemplatesError struct{ cause error }

func (e *unrenderableTemplatesError) Error() string { return e.cause.Error() }

func (e *unrenderableTemplatesError) Unwrap() error { return e.cause }

// templatesUnrenderable reports whether err is that verdict.
func templatesUnrenderable(err error) bool {
	var unrenderable *unrenderableTemplatesError
	return errors.As(err, &unrenderable)
}

// explainWebviewFailure says what actually went wrong when a webview chunk
// hands out WeChat's newer compiled templates. Those resolve to a chain of
// functions instead of a node tree, and marshalling the export fails with
// encoding/json's "unsupported type: func(...)", which tells the user nothing
// about the app in front of them. The original error is appended so the cause
// stays visible even if encoding/json's wording ever changes.
//
// This only chooses the wording — what counts as a verdict is
// unrenderableTemplatesError, never a string match.
func explainWebviewFailure(err error) error {
	if err == nil || !strings.Contains(err.Error(), "unsupported type: func(") {
		return err
	}
	return fmt.Errorf("该小程序的页面模板由微信新版编译模板运行时（__wxCodeSpace__）生成，本工具还原不了，反编译会整体失败；原始错误：%w", err)
}

func newRestoreRuntime() *goja.Runtime {
	vm := goja.New()
	patch := `
var console = {log:function(){},warn:function(){},error:function(){}};
var window = this;
window.screen = {width:375,height:667};
window.__rpxRecalculatingFuncs__ = [];
var navigator = {userAgent:"iPhone"};
var document = {
  head: {appendChild:function(){}},
  getElementsByTagName:function(){return [{appendChild:function(){}}];},
  createElement:function(){return {style:{},setAttribute:function(){},appendChild:function(){}};},
  addEventListener:function(){},
  dispatchEvent:function(){}
};
var CustomEvent = function(type, init){this.type=type;this.detail=init&&init.detail;};
var global = this;
var outerGlobal = this;
var __wxAppCode__ = {};
var __wxAppData = {};
var __WXML_GLOBAL__ = {entrys:{},defines:{},modules:{},ops:[],ops_cached:{},ops_set:{},ops_init:{}};
var __GWX_GLOBAL__ = {};
var __vd_version_info__ = {};
var setCssToHead = function(){};
var require = function(){};
var define = function(){};
var Component = function(){};
var Behavior = function(){};
`
	if _, err := vm.RunString(patch); err != nil {
		panic(err)
	}
	return vm
}

func wrapRestoreScript(source string) string {
	return "try {\n" + source + "\n} catch (e) {}\n"
}

func resolveWebviewResult(vm *goja.Runtime, value goja.Value) (goja.Value, error) {
	for depth := 0; depth < 32 && value != nil && value.ExportType() != nil && value.ExportType().Kind() == reflect.Func; depth++ {
		fn, ok := goja.AssertFunction(value)
		if !ok {
			break
		}
		next, err := fn(goja.Undefined(), vm.ToValue(map[string]any{}), vm.ToValue(map[string]any{}), vm.GlobalObject())
		if err != nil {
			return nil, err
		}
		value = next
	}
	if value != nil && value.ExportType() != nil && value.ExportType().Kind() == reflect.Func {
		return nil, fmt.Errorf("chunk render function recursion limit exceeded")
	}
	return value, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
