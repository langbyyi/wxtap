package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/langbyyi/wxtap/desktop/internal/update"
)

// The frontend cannot read a Go constant, so FeedbackView.vue hardcodes the
// release repository in two URLs, and nothing else ties those strings to
// update.ReleaseRepo — renaming the distribution repository would leave the
// feedback page silently pointing at the old repo without failing any build,
// so the pairing is asserted here.
func TestFeedbackViewPointsWhereTheReleasePublishes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("frontend", "src", "views", "FeedbackView.vue"))
	if err != nil {
		t.Fatalf("read the feedback view: %v", err)
	}
	source := string(raw)

	feedback := "https://raw.githubusercontent.com/" + update.ReleaseRepo + "/main/feedback.md"
	if !strings.Contains(source, feedback) {
		t.Fatalf("前端反馈页硬编码的仓库地址与 update.ReleaseRepo 漂移：FeedbackView.vue 中找不到 %q。仓库更名后须同步更新该文件里的 feedbackUrl（raw 文档地址）这一处硬编码字符串", feedback)
	}

	issues := "https://github.com/" + update.ReleaseRepo + "/issues"
	if !strings.Contains(source, issues) {
		t.Fatalf("前端反馈页硬编码的仓库地址与 update.ReleaseRepo 漂移：FeedbackView.vue 中找不到 %q。仓库更名后须同步更新该文件里的 issuesUrl（Issues 地址）这一处硬编码字符串", issues)
	}
}
