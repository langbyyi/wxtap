package extract

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/dop251/goja"
)

var pageConfigPattern = regexp.MustCompile(`__wxAppCode__\s*\[\s*["']([^"']+\.json)["']\s*\]\s*=\s*\{`)

func splitPageConfigs(root string, source []byte) ([]string, error) {
	matches := pageConfigPattern.FindAllSubmatchIndex(source, -1)
	if len(matches) == 0 {
		return nil, nil
	}
	appCode := map[string]any{}
	vm := goja.New()
	timer := time.AfterFunc(wxmlRestoreTimeout, func() {
		vm.Interrupt("page config restoration timed out")
	})
	defer timer.Stop()
	if err := vm.Set("__wxAppCode__", &appCode); err != nil {
		return nil, err
	}

	var written []string
	for _, match := range matches {
		if len(match) < 4 {
			continue
		}
		name := string(source[match[2]:match[3]])
		openBrace := match[1] - 1
		closeBrace, ok := matchingDelimiter(string(source), openBrace, '{', '}')
		if !ok {
			return written, fmt.Errorf("unterminated page config %s", name)
		}
		if _, err := vm.RunString(string(source[match[0]:closeBrace+1]) + ";"); err != nil {
			return written, fmt.Errorf("parse page config %s: %w", name, err)
		}
		value, ok := appCode[name]
		if !ok {
			continue
		}
		normalized, err := normalizeExport(value)
		if err != nil {
			return written, fmt.Errorf("normalize page config %s: %w", name, err)
		}
		data, err := json.MarshalIndent(normalized, "", "  ")
		if err != nil {
			return written, fmt.Errorf("format page config %s: %w", name, err)
		}
		target, err := safeChildPath(root, name)
		if err != nil {
			return written, err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return written, err
		}
		if err := os.WriteFile(target, append(data, '\n'), 0o600); err != nil {
			return written, err
		}
		written = append(written, target)
	}
	return written, nil
}
