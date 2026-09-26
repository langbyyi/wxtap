package main

import "testing"

// The dialog is never shown on the platforms the test suite runs on, but the
// quoting it depends on is plain string work and is checked everywhere.
func TestAppleScriptLiteralQuotesWhatItEmbeds(t *testing.T) {
	for input, want := range map[string]string{
		"plain":           `"plain"`,
		`say "hi"`:        `"say \"hi\""`,
		`back\slash`:      `"back\\slash"`,
		"two\nlines":      `"two\nlines"`,
		"strip\rcarriage": `"stripcarriage"`,
	} {
		if got := appleScriptLiteral(input); got != want {
			t.Errorf("appleScriptLiteral(%q) = %s, want %s", input, got, want)
		}
	}
}
