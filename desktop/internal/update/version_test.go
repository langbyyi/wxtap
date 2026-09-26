package update

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		left  string
		right string
		want  int
	}{
		{"v2.1.0", "v2.0.0", 1},
		{"v2.0.0", "v2.0.0", 0},
		{"v1.9.0", "v2.0.0", -1},
		{"v2.0.0", "v2.0.1", -1},
		// The prefix is presentation, not identity: the previous string
		// comparison reported a phantom update whenever the publisher wrote
		// "2.0.0" while the shell called itself "v2.0.0".
		{"v2.0.0", "2.0.0", 0},
		{"V2.0.0", "v2.0.0", 0},
		// Numeric, not lexical: 10 outranks 9.
		{"v2.10.0", "v2.9.0", 1},
		// A missing segment reads as zero, so v2.0 and v2.0.0 are the same release.
		{"v2.0", "v2.0.0", 0},
		{"v2.0.0.1", "v2.0.0", 1},
		// A pre-release sorts before the release it precedes.
		{"v2.0.0-rc1", "v2.0.0", -1},
		{"v2.0.0", "v2.0.0-rc1", 1},
		{"v2.0.0-rc1", "v2.0.0-rc2", -1},
		// Build metadata carries no ordering.
		{"v2.0.0+build9", "v2.0.0", 0},
		{"  v3.0.0  ", "v2.9.9", 1},
	}
	for _, testCase := range cases {
		if got := CompareVersions(testCase.left, testCase.right); got != testCase.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", testCase.left, testCase.right, got, testCase.want)
		}
		// The relation must hold in both directions.
		if got := CompareVersions(testCase.right, testCase.left); got != -testCase.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", testCase.right, testCase.left, got, -testCase.want)
		}
	}
}

func TestHasUpdate(t *testing.T) {
	cases := []struct {
		current string
		latest  string
		want    bool
	}{
		{"v2.0.0", "v2.1.0", true},
		{"v2.0.0", "v2.0.0", false},
		// A republished or rolled-back older version must not read as an update.
		{"v2.0.0", "v1.9.0", false},
		{"v2.0.0", "v2.0.0-rc1", false},
		// A missing or empty publication is a broken document, not a release.
		{"v2.0.0", "", false},
		{"v2.0.0", "   ", false},
		// An unknown current version cannot outrank anything.
		{"", "v2.0.0", true},
	}
	for _, testCase := range cases {
		if got := HasUpdate(testCase.current, testCase.latest); got != testCase.want {
			t.Errorf("HasUpdate(%q, %q) = %v, want %v", testCase.current, testCase.latest, got, testCase.want)
		}
	}
}
