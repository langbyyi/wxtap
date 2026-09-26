package update

import (
	"strconv"
	"strings"
)

// HasUpdate reports whether latest is a newer release than current. An empty
// latest (a broken or missing publication) is never an update; anything else
// is ordered by CompareVersions, so a republished older version no longer
// reads as "有更新" the way the previous string-inequality check did.
func HasUpdate(current, latest string) bool {
	return strings.TrimSpace(latest) != "" && CompareVersions(latest, current) > 0
}

// CompareVersions orders two dotted version strings, returning -1, 0 or 1.
// A leading "v" and any "+build" suffix are ignored. Numeric segments are
// compared as numbers, so v2.10.0 outranks v2.9.0. A "-pre" suffix sorts
// before its own release, so v2.0.0-rc1 < v2.0.0.
func CompareVersions(a, b string) int {
	left, right := parseVersion(a), parseVersion(b)
	segments := len(left.numbers)
	if len(right.numbers) > segments {
		segments = len(right.numbers)
	}
	for index := 0; index < segments; index++ {
		if order := compareInt64(segmentAt(left.numbers, index), segmentAt(right.numbers, index)); order != 0 {
			return order
		}
	}
	// Equal numeric cores: a final release outranks its pre-releases.
	switch {
	case left.pre == right.pre:
		return 0
	case left.pre == "":
		return 1
	case right.pre == "":
		return -1
	}
	return strings.Compare(left.pre, right.pre)
}

type parsedVersion struct {
	numbers []int64
	pre     string
}

// parseVersion splits a version into its numeric segments and pre-release
// suffix. Parsing stops at the first segment that is not a number, so an
// unparseable tail is dropped rather than guessed at.
func parseVersion(version string) parsedVersion {
	trimmed := strings.TrimSpace(version)
	trimmed = strings.TrimPrefix(strings.TrimPrefix(trimmed, "v"), "V")
	if index := strings.IndexByte(trimmed, '+'); index >= 0 {
		trimmed = trimmed[:index]
	}
	pre := ""
	if index := strings.IndexByte(trimmed, '-'); index >= 0 {
		pre = trimmed[index+1:]
		trimmed = trimmed[:index]
	}
	var numbers []int64
	for _, part := range strings.Split(trimmed, ".") {
		number, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil {
			break
		}
		numbers = append(numbers, number)
	}
	return parsedVersion{numbers: numbers, pre: pre}
}

// segmentAt reads a numeric segment, treating a missing one as zero so
// v2.0 and v2.0.0 compare equal.
func segmentAt(numbers []int64, index int) int64 {
	if index >= len(numbers) {
		return 0
	}
	return numbers[index]
}

func compareInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
