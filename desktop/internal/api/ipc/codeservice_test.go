package ipc

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeCodeTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustWrite := func(rel, content string) {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("b.js", "var b=1;")
	mustWrite("A.txt", "hello")
	mustWrite("sub/inner.js", "var i=1;")
	mustWrite("sub/nested/deep.js", "var d=1;")
	mustWrite(".hidden", "x")
	mustWrite("empty/.keep", "")
	if err := os.Remove(filepath.Join(root, "empty", ".keep")); err != nil {
		t.Fatal(err)
	}
	return root
}

// The tree lists directories first, then
// case-insensitive names, dotfiles skipped, one expansion level deep.
func TestBuildFileTreeTopLevel(t *testing.T) {
	root := writeCodeTree(t)
	tree := BuildFileTree(root, 1)

	var names []string
	for _, node := range tree {
		names = append(names, node.Name)
	}
	// "empty" and "sub" are dirs and must come before the files; "A.txt"
	// sorts before "b.js" case-insensitively; ".hidden" is skipped.
	if strings.Join(names, ",") != "empty,sub,A.txt,b.js" {
		t.Fatalf("unexpected tree order: %v", names)
	}
}

func TestBuildFileTreeDepthExpansion(t *testing.T) {
	root := writeCodeTree(t)
	tree := BuildFileTree(root, 1)

	var sub *FileNode
	for i := range tree {
		if tree[i].Name == "sub" {
			sub = &tree[i]
		}
	}
	if sub == nil {
		t.Fatal("sub missing from tree")
	}
	// Depth 1 expands one level: sub's children are listed (nested +
	// inner.js), but the nested directory inside them has no children
	// (omitempty) until expandDir.
	if len(sub.Children) != 2 {
		t.Fatalf("sub children: %+v", sub.Children)
	}
	var nested *FileNode
	for i := range sub.Children {
		if sub.Children[i].Name == "nested" {
			nested = &sub.Children[i]
		}
	}
	if nested == nil || nested.Children != nil {
		t.Fatalf("nested should be unexpanded at depth 1: %+v", nested)
	}

	expanded := ExpandDir(filepath.Join(root, "sub"))
	if len(expanded) != 2 || expanded[0].Name != "nested" {
		t.Fatalf("expandDir: %+v", expanded)
	}
}

// An empty directory expanded at depth > 0 must marshal as [] (
// returns an empty list), while an unexpanded one omits the field.
func TestBuildFileTreeEmptyDirMarshalsAsArray(t *testing.T) {
	root := writeCodeTree(t)
	tree := BuildFileTree(root, 1)

	var empty *FileNode
	for i := range tree {
		if tree[i].Name == "empty" {
			empty = &tree[i]
		}
	}
	if empty == nil {
		t.Fatal("empty missing from tree")
	}
	blob, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	if string(blob) == "" || !strings.Contains(string(blob), `"children":[]`) {
		t.Fatalf("empty dir should marshal children as []: %s", blob)
	}

	// A missing directory yields nil so the caller returns {tree: []}-like
	// emptiness without marshalling bogus nodes.
	if got := BuildFileTree(filepath.Join(root, "nope"), 1); got != nil {
		t.Fatalf("missing dir: %+v", got)
	}
}

func TestReadCodeFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "app.js")
	if err := os.WriteFile(path, []byte("var a=1;"), 0o644); err != nil {
		t.Fatal(err)
	}

	file := ReadCodeFile(path)
	if file.Content != "var a=1;" || file.Language != "javascript" || file.Size != 8 {
		t.Fatalf("read file: %+v", file)
	}

	missing := ReadCodeFile(filepath.Join(root, "nope.js"))
	if missing.Content != "" || missing.Size != 0 || missing.Language != "text" {
		t.Fatalf("missing file: %+v", missing)
	}
}

// An image is inlined as a data URL: the viewer draws a picture instead of
// feeding PNG bytes to the highlighter.
func TestReadCodeFileInlinesImages(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "logo.png")
	// 真实 PNG 头 + 一段带 NUL 的负载（图片本来就是二进制）。
	blob := append([]byte("\x89PNG\r\n\x1a\n"), []byte{0, 0, 0, 13, 0x49, 0x48, 0x44, 0x52}...)
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatal(err)
	}

	file := ReadCodeFile(path)
	if file.Kind != "image" {
		t.Fatalf("png should read as image: %+v", file)
	}
	if !strings.HasPrefix(file.DataURL, "data:image/png;base64,") {
		t.Fatalf("data URL missing: %q", file.DataURL)
	}
	if file.Content != "" {
		t.Fatalf("image should not carry source content: %q", file.Content)
	}
	if file.Size != int64(len(blob)) {
		t.Fatalf("size should be the real file size, got %d", file.Size)
	}
	if !strings.Contains(file.DataURL, base64.StdEncoding.EncodeToString(blob)) {
		t.Fatal("data URL should carry the file bytes")
	}
}

// An image past the inline cap keeps the image kind so the viewer still
// explains itself instead of showing mojibake.
func TestReadCodeFileLeavesOversizeImageUninlined(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "huge.gif")
	if err := os.WriteFile(path, make([]byte, inlineImageLimit+1), 0o644); err != nil {
		t.Fatal(err)
	}

	file := ReadCodeFile(path)
	if file.Kind != "image" || file.DataURL != "" {
		t.Fatalf("oversize image should stay an uninlined image: %+v", file)
	}
	if !strings.Contains(file.Content, "超过") {
		t.Fatalf("oversize image should say why it is not shown: %q", file.Content)
	}
}

// Non-image binaries (fonts, archives, wasm) get a one-line description
// rather than replacement characters in the source pane.
func TestReadCodeFileDescribesBinaryFiles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "app.wasm")
	if err := os.WriteFile(path, []byte("\x00asm\x01\x00\x00\x00"), 0o644); err != nil {
		t.Fatal(err)
	}

	file := ReadCodeFile(path)
	if file.Kind != "binary" {
		t.Fatalf("wasm should read as binary: %+v", file)
	}
	if !strings.Contains(file.Content, "二进制文件") {
		t.Fatalf("binary should be described: %q", file.Content)
	}
	if file.Language != "text" {
		t.Fatalf("binary should not claim a language, got %q", file.Language)
	}
}

// ReadCodeFile marks source files as text so the viewer keeps the code pane.
func TestReadCodeFileKeepsSourceAsText(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "app.js")
	if err := os.WriteFile(path, []byte("var a=1;"), 0o644); err != nil {
		t.Fatal(err)
	}
	if file := ReadCodeFile(path); file.Kind != "text" || file.Language != "javascript" {
		t.Fatalf("source should stay text: %+v", file)
	}
}

// Bodies larger than 1MB are truncated with a truncation marker appended.
func TestReadCodeFileTruncatesLargeFiles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "big.js")
	content := strings.Repeat("x", 1024*1024+100)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	file := ReadCodeFile(path)
	if !file.Truncated {
		t.Fatal("large file should be marked truncated")
	}
	if file.Size != int64(len(content)) {
		t.Fatalf("size should be the real file size, got %d", file.Size)
	}
	if len(file.Content) >= len(content) {
		t.Fatal("content should be truncated")
	}
	if !strings.Contains(file.Content, "文件过大，已截断显示") {
		t.Fatalf("truncation marker missing: %q", file.Content[len(file.Content)-60:])
	}
}

func TestSearchCode(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.js"), []byte("var token = 1;\nvar other = 2;\nTOKEN_URL = 'x';\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("no match here\ntoken again\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	hits, truncated, err := SearchCode(root, "token", false)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Fatal("a two-hit search is not truncated")
	}
	if len(hits) != 2 || hits[0].Line != 1 || hits[1].Line != 2 || hits[1].File != filepath.Join(root, "b.txt") {
		t.Fatalf("substring search: %+v", hits)
	}

	regexHits, _, err := SearchCode(root, `var \w+ = \d`, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(regexHits) != 2 {
		t.Fatalf("regex search: %+v", regexHits)
	}

	if _, _, err := SearchCode(root, "([unclosed", true); err == nil {
		t.Fatal("bad regex should error")
	}

	if hits, _, err := SearchCode(root, "", false); err != nil || hits != nil {
		t.Fatalf("empty query should return no hits: %v %v", hits, err)
	}
}

// Binary files (images, fonts, archives) sit next to the code in a decompiled
// package; their bytes must not turn into matches.
func TestSearchCodeSkipsBinaryFiles(t *testing.T) {
	root := t.TempDir()
	// A PNG header followed by a NUL byte, with the needle inside the binary.
	binary := append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00}, []byte("needle in a binary")...)
	if err := os.WriteFile(filepath.Join(root, "icon.png"), binary, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.js"), []byte("needle here\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	hits, _, err := SearchCode(root, "needle", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].File != filepath.Join(root, "app.js") {
		t.Fatalf("binary file should not be searched: %+v", hits)
	}
}

func TestSearchCodeCapAndTruncation(t *testing.T) {
	root := t.TempDir()
	var lines []string
	for i := 0; i < 600; i++ {
		lines = append(lines, "hit")
	}
	if err := os.WriteFile(filepath.Join(root, "many.txt"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	hits, truncated, err := SearchCode(root, "hit", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 500 {
		t.Fatalf("hit cap: %d", len(hits))
	}
	if !truncated {
		t.Fatal("a capped search must report itself as truncated")
	}

	long := strings.Repeat("y", 300)
	if err := os.WriteFile(filepath.Join(root, "long.txt"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	longHits, longTruncated, err := SearchCode(root, "y", false)
	if err != nil {
		t.Fatal(err)
	}
	// 超长行按匹配位置窗口化：摘录以省略号标注截断、总量不超过行上限 + 两个
	// 标记，且列号指向匹配处（这个用例里命中在行首）。
	if len(longHits) != 1 {
		t.Fatalf("line truncation hit count: %+v", longHits)
	}
	if excerpt := []rune(longHits[0].Text); len(excerpt) > searchLineCap+2 {
		t.Fatalf("excerpt over line cap: %d runes", len(excerpt))
	} else if !strings.HasSuffix(longHits[0].Text, "…") {
		t.Fatalf("a windowed excerpt must mark its tail: %q", longHits[0].Text)
	}
	if longHits[0].Column != 1 {
		t.Fatalf("column = %d, want 1", longHits[0].Column)
	}
	if longTruncated {
		t.Fatal("a single hit is not a truncated result set")
	}

	// 命中在压缩长行的中后段时，摘录必须围绕匹配处，而不是行首样板。
	buried := strings.Repeat("a", 5000) + "needle" + strings.Repeat("b", 5000)
	if err := os.WriteFile(filepath.Join(root, "buried.txt"), []byte(buried), 0o644); err != nil {
		t.Fatal(err)
	}
	buriedHits, _, err := SearchCode(root, "needle", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(buriedHits) != 1 {
		t.Fatalf("buried hit count: %d", len(buriedHits))
	}
	if !strings.HasPrefix(buriedHits[0].Text, "…") || !strings.Contains(buriedHits[0].Text, "needle") {
		t.Fatalf("excerpt must be windowed around the match: %q", buriedHits[0].Text)
	}
	// Column 指向匹配在摘录内的 1 基位置（前导省略号占第 1 位）。
	excerpt := []rune(buriedHits[0].Text)
	if buriedHits[0].Column < 1 || buriedHits[0].Column > len(excerpt) ||
		string(excerpt[buriedHits[0].Column-1:buriedHits[0].Column-1+6]) != "needle" {
		t.Fatalf("column %d does not locate the match in %q", buriedHits[0].Column, buriedHits[0].Text)
	}
}

// A directory the process cannot read must skip only itself, not abort the
// whole search - a silent truncation is a false negative for a scanner.
func TestSearchCodeSkipsUnreadableDirs(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "later.txt"), []byte("needle here\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Sorted order puts aadeny/ (unreadable via a deny ACE) before later.txt,
	// so an aborting walk error would hide the match below.
	denied := filepath.Join(root, "aadeny")
	if err := os.MkdirAll(denied, 0o755); err != nil {
		t.Fatal(err)
	}
	// Deny read on Windows (ACL) and elsewhere (POSIX mode bits); both are
	// reverted before the temp dir is cleaned up.
	unreadable, restore := makeUnreadable(denied)
	defer restore()
	if !unreadable {
		t.Skip("host cannot make a directory unreadable for this process")
	}

	hits, _, err := SearchCode(root, "needle", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].File != filepath.Join(root, "later.txt") {
		t.Fatalf("search truncated by an unreadable directory: %+v", hits)
	}
}

// makeUnreadable denies this process read access to dir, returning whether
// the denial took effect plus a restore func. Windows uses a deny ACE
// (POSIX chmod is a no-op there); other platforms use mode 0.
func makeUnreadable(dir string) (bool, func()) {
	if runtime.GOOS == "windows" {
		if err := exec.Command("icacls", dir, "/deny", "*S-1-1-0:(OI)(CI)(R)").Run(); err != nil {
			return false, func() {}
		}
		restore := func() { _ = exec.Command("icacls", dir, "/remove", "*S-1-1-0").Run() }
		if _, err := os.ReadDir(dir); err == nil {
			return false, restore
		}
		return true, restore
	}
	original, err := os.Stat(dir)
	if err != nil {
		return false, func() {}
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		return false, func() {}
	}
	restore := func() { _ = os.Chmod(dir, original.Mode().Perm()) }
	if _, err := os.ReadDir(dir); err == nil {
		// Running as root ignores mode bits.
		return false, restore
	}
	return true, restore
}

// code.formatAll walks a directory and formats every file whose extension
// passes FormatAllExtension, then hands ipc.FormatLanguage to the Core. A
// whitelisted extension without a language would format nothing while still
// reporting success, so the two tables have to agree.
func TestFormatWhitelistsAgree(t *testing.T) {
	for ext := range formatAllExtensions {
		language, ok := FormatLanguage(ext)
		if !ok || language == "" {
			t.Fatalf("formatAll extension %q has no language mapping", ext)
		}
	}
	if _, ok := FormatLanguage(".ts"); !ok {
		t.Fatal(".ts must stay formattable through code.formatFile")
	}
	if FormatAllExtension(".ts") || FormatAllExtension(".xml") {
		t.Fatal(".ts/.xml are intentionally excluded from formatAll")
	}
	if FormatAllExtension(".txt") || FormatAllExtension(".JS ") {
		t.Fatal("unlisted extensions must not be formatted")
	}
	if language, ok := FormatLanguage(".JS"); !ok || language != "javascript" {
		t.Fatalf("extension lookup must be case-insensitive: %q %v", language, ok)
	}
}
