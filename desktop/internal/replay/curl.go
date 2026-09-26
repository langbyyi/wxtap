package replay

import (
	"sort"
	"strings"
)

// BuildCurl renders the target as two equivalent curl command lines: bash for
// POSIX shells (every value wrapped in single quotes, embedded quotes escaped
// as '\”) and cmd for Windows cmd.exe (every value wrapped in double quotes,
// embedded quotes escaped as \" with MSVC backslash-doubling). Quoting carries
// the escaping duty: inside double quotes cmd treats & | < > ( ) ^ as literal,
// so no caret prefixes are emitted — a caret there would travel to the server
// as data. Two residual cmd caveats are inherent to the shell and documented
// rather than hidden: %VAR% still expands inside quotes, and a body containing
// a raw newline cannot be represented on one cmd line (write it to a file and
// use --data-raw @file instead). -k mirrors the client's TLS-verify-off
// stance; no --http1.1 or other noise is added.
func (t Target) BuildCurl() (bash string, cmd string) {
	bashParts := []string{"curl", "-k"}
	cmdParts := []string{"curl", "-k"}
	method := strings.ToUpper(strings.TrimSpace(t.Method))
	if method == "" {
		method = "GET"
	}
	if method != "GET" || t.Body != "" {
		bashParts = append(bashParts, "-X", method)
		cmdParts = append(cmdParts, "-X", method)
	}
	bashParts = append(bashParts, bashToken(t.URL))
	cmdParts = append(cmdParts, cmdToken(t.URL))
	// Header order is sorted: map iteration is random and both humans and
	// tests read these lines verbatim.
	keys := make([]string, 0, len(t.Headers))
	for key := range t.Headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		bashParts = append(bashParts, "-H", bashToken(key+": "+t.Headers[key]))
		cmdParts = append(cmdParts, "-H", cmdToken(key+": "+t.Headers[key]))
	}
	if t.Body != "" {
		bashParts = append(bashParts, "--data-raw", bashToken(t.Body))
		cmdParts = append(cmdParts, "--data-raw", cmdToken(t.Body))
	}
	return strings.Join(bashParts, " "), strings.Join(cmdParts, " ")
}

// bashToken wraps s in single quotes; a literal ' becomes '\” which closes
// the quote, emits an escaped quote and reopens. Spaces, newlines and Unicode
// survive untouched inside single quotes.
func bashToken(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// cmdToken wraps s in double quotes. An embedded " becomes \" and every run
// of backslashes directly before a quote is doubled, which is what the MSVC
// argv convention (and therefore curl.exe) expects: "a\" already reads as an
// escaped quote otherwise.
func cmdToken(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, r := range s {
		if r == '\\' {
			slashes++
			b.WriteByte('\\')
			continue
		}
		if r == '"' {
			for i := 0; i < slashes; i++ {
				b.WriteByte('\\')
			}
			b.WriteString(`\"`)
		} else {
			b.WriteRune(r)
		}
		slashes = 0
	}
	// The closing quote gets the same doubling for a trailing backslash run.
	for i := 0; i < slashes; i++ {
		b.WriteByte('\\')
	}
	b.WriteByte('"')
	return b.String()
}
