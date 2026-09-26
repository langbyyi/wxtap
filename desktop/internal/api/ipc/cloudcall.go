package ipc

import (
	"encoding/json"
	"fmt"
)

// CloudCallExpression builds the JS that invokes a cloud function through
// the injected cloud hook. cloudAudit.callFunction always resolves (it wraps
// failures and its own 10s timeout into the result object), so callers should
// evaluate it with awaitPromise enabled.
func CloudCallExpression(name string, data any) (string, error) {
	dataJSON, err := jsonMarshal(data)
	if err != nil {
		return "", fmt.Errorf("marshal cloud call data: %w", err)
	}
	return fmt.Sprintf("window.cloudAudit.callFunction('%s', %s)", escapeJS(name), dataJSON), nil
}

// CloudContainerExpression builds the JS that calls wx.cloud.callContainer
// wrapped in a Promise so awaitPromise can settle it. Success and failure both
// resolve to objects (parsed from the page-side JSON.stringify),
// never a bare JSON string. An empty method defaults to POST.
func CloudContainerExpression(path string, method string, header any, data any) (string, error) {
	if method == "" {
		method = "POST"
	}
	headerJSON, err := jsonMarshal(header)
	if err != nil {
		return "", fmt.Errorf("marshal callContainer header: %w", err)
	}
	dataJSON, err := jsonMarshal(data)
	if err != nil {
		return "", fmt.Errorf("marshal callContainer data: %w", err)
	}
	return fmt.Sprintf(
		"new Promise((resolve) => { wx.cloud.callContainer({ path: '%s', method: '%s', header: %s, data: %s, success: (res) => resolve(res), fail: (err) => resolve({errMsg: (err && err.errMsg) || 'fail'}) }); })",
		escapeJS(path), escapeJS(method), headerJSON, dataJSON,
	), nil
}

// NormalizeCloudContainerResult parses the polled
// __cloudContainerResult value: if Runtime.evaluate somehow still returns a
// JSON string, parse it to an object; otherwise pass the value through.
func NormalizeCloudContainerResult(value any) any {
	switch v := value.(type) {
	case string:
		var parsed any
		if err := json.Unmarshal([]byte(v), &parsed); err == nil {
			return parsed
		}
		return map[string]any{"raw": v}
	default:
		return value
	}
}
