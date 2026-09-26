package main

import "strings"

// appleScriptLiteral quotes a value as an AppleScript string literal, so a
// message containing quotes, backslashes or newlines cannot break the script
// osascript is handed. It lives outside the platform files so that it can be
// tested on every platform, including the ones that never call it.
func appleScriptLiteral(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", "")
	return `"` + replacer.Replace(value) + `"`
}
