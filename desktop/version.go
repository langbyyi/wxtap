package main

import (
	_ "embed"
	"encoding/json"
	"log"
	"strings"
)

// wailsJSON is the project descriptor Wails itself reads. Embedding it makes
// wails.json the *single* source of the release version: the frontend already
// reads info.productVersion from this file at build time
// (desktop/frontend/vite.config.ts), and the shell now does the same instead of
// keeping a matching literal in Go. Cutting a release edits this one file.
//
//go:embed wails.json
var wailsJSON []byte

// appVersion is the desktop shell version reported by update.checkVersion, by
// the MCP handshake and on the settings page. Spelled with the leading "v" the
// shell has always used for it.
var appVersion = "v" + versionFromWailsJSON(wailsJSON)

// versionFromWailsJSON reads info.productVersion, or "" when the descriptor is
// unusable. Taking the bytes as an argument keeps the unparseable case testable.
func versionFromWailsJSON(descriptor []byte) string {
	var parsed struct {
		Info struct {
			ProductVersion string `json:"productVersion"`
		} `json:"info"`
	}
	if err := json.Unmarshal(descriptor, &parsed); err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.TrimSpace(parsed.Info.ProductVersion), "v")
}

// verifyEmbeddedVersion reports whether the embedded descriptor yielded a
// version that looks like a release. It is checked at startup and by a test:
// an unusable descriptor means the binary was built from a broken tree, and a
// fabricated "1.0.0" would make update.checkVersion answer confidently wrong.
func verifyEmbeddedVersion() bool {
	version := strings.TrimPrefix(appVersion, "v")
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func init() {
	if !verifyEmbeddedVersion() {
		log.Printf("wails.json 的 info.productVersion 无法解析（当前 %q）；版本上报为 unknown", appVersion)
		appVersion = "unknown"
	}
}
