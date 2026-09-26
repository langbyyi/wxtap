package assets

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/traffic"
)

// maxFileRead is the per-file analysis window (2 MB): files bigger than it
// are skipped entirely and reads never exceed it.
const maxFileRead = 2_000_000

const (
	maxSources = 8 // sources kept per asset before the "+N" marker
	maxTags    = 5 // query-key tags kept per asset
	// trailingPunct is trimmed off the end of every URL match: source code
	// and JSON glue sentence or list punctuation onto string literals.
	trailingPunct = ".,;:)]}'\""
)

// urlPattern matches http(s) and ws(s) URLs. A match ends at whitespace,
// quotes, angle brackets, parentheses or a backslash — the characters that
// reliably terminate a URL inside JS/JSON/WXML string literals and markup.
var urlPattern = regexp.MustCompile(`(?:https?|wss?)://[^\s"'<>()\\]+`)

// staticExts are the path extensions classified as static resources.
var staticExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true,
	".svg": true, ".ico": true, ".css": true, ".js": true, ".mjs": true,
	".mp3": true, ".mp4": true, ".wav": true, ".woff": true, ".woff2": true,
	".ttf": true, ".eot": true, ".wasm": true, ".webm": true,
}

// parsedURL is one observed URL normalized for dedup: fragment stripped,
// trailing punctuation trimmed, host lowercased, empty path collapsed to "/"
// and the query reduced to its keys.
type parsedURL struct {
	scheme  string
	host    string
	path    string
	display string
	keys    []string
}

// parseObservedURL normalizes one raw URL match; ok is false when the string
// does not carry a usable host (bare schemes, template placeholders).
func parseObservedURL(raw string) (parsedURL, bool) {
	s := raw
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimRight(s, trailingPunct)
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return parsedURL{}, false
	}
	p := parsedURL{
		scheme: strings.ToLower(u.Scheme),
		host:   strings.ToLower(u.Host),
		path:   u.EscapedPath(),
		keys:   queryKeys(u.RawQuery),
	}
	if p.path == "" {
		p.path = "/"
	}
	p.display = p.scheme + "://" + p.host + p.path
	if len(p.keys) > 0 {
		p.display += "?" + strings.Join(p.keys, "&")
	}
	return p, true
}

// queryKeys splits a raw query into its unique keys, dropping empty ones
// ("?a=1&b=2" -> [a b]); values are not kept so the inventory never stores
// captured parameter data.
func queryKeys(rawQuery string) []string {
	if rawQuery == "" {
		return nil
	}
	seen := make(map[string]bool)
	var keys []string
	for _, pair := range strings.Split(rawQuery, "&") {
		key := pair
		if i := strings.IndexByte(pair, '='); i >= 0 {
			key = pair[:i]
		}
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	return keys
}

// pathExt returns the lowercased extension of the last URL path segment.
func pathExt(p string) string {
	seg := p
	if i := strings.LastIndexByte(seg, '/'); i >= 0 {
		seg = seg[i+1:]
	}
	if j := strings.LastIndexByte(seg, '.'); j >= 0 {
		return strings.ToLower(seg[j:])
	}
	return ""
}

// classifyKind: ws(s) schemes are websocket endpoints, known media/font/
// script extensions are static resources, everything else is an API call.
func classifyKind(scheme, path string) string {
	switch scheme {
	case "ws", "wss":
		return KindWS
	}
	if staticExts[pathExt(path)] {
		return KindStatic
	}
	return KindAPI
}

// dedupKey is the asset identity: query values are deliberately absent, so
// the same path hit with different parameters merges into one asset.
func dedupKey(method, scheme, host, path string) string {
	return method + "|" + scheme + "|" + host + "|" + path
}

// draft accumulates observations for one dedup key before it becomes an Asset.
type draft struct {
	kind, method string
	scheme       string
	host         string
	path         string
	display      string
	sources      []Source
	srcSeen      map[Source]bool
	hits         int
	trafficSeen  bool
	first, last  time.Time
	tags         []string
	tagSeen      map[string]bool
}

func newDraft(kind, method string, p parsedURL) *draft {
	return &draft{
		kind:    kind,
		method:  method,
		scheme:  p.scheme,
		host:    p.host,
		path:    p.path,
		display: p.display,
		srcSeen: make(map[Source]bool),
		tagSeen: make(map[string]bool),
	}
}

func (d *draft) addSource(s Source) {
	if d.srcSeen[s] {
		return
	}
	d.srcSeen[s] = true
	d.sources = append(d.sources, s)
}

func (d *draft) addHits(n int) { d.hits += n }

// observe folds a timestamp into the first/last range. Zero times carry no
// information (code sources have no timestamp) and are ignored, so a merged
// code+traffic asset keeps the traffic window instead of degrading to the
// zero time.
func (d *draft) observe(t time.Time) {
	if t.IsZero() {
		return
	}
	if d.first.IsZero() || t.Before(d.first) {
		d.first = t
	}
	if d.last.IsZero() || t.After(d.last) {
		d.last = t
	}
}

func (d *draft) addTag(tag string) {
	if tag == "" || d.tagSeen[tag] {
		return
	}
	d.tagSeen[tag] = true
	d.tags = append(d.tags, tag)
}

func (d *draft) addQueryKeys(keys []string) {
	for _, k := range keys {
		d.addTag("q:" + k)
	}
}

// merge folds another draft carrying the same dedup key into d.
func (d *draft) merge(o *draft) {
	d.addHits(o.hits)
	d.trafficSeen = d.trafficSeen || o.trafficSeen
	for _, s := range o.sources {
		d.addSource(s)
	}
	for _, t := range o.tags {
		d.addTag(t)
	}
	d.observe(o.first)
	d.observe(o.last)
}

// asset freezes the draft: caps applied, stable ID computed.
func (d *draft) asset() Asset {
	tags := append([]string(nil), d.tags...)
	sort.Strings(tags)
	if len(tags) > maxTags {
		tags = tags[:maxTags]
	}
	return Asset{
		ID:          assetID(d.kind, d.method, d.scheme, d.host, d.path),
		Kind:        d.kind,
		URL:         d.display,
		Host:        d.host,
		Path:        d.path,
		Method:      d.method,
		Sources:     capSources(d.sources),
		Hits:        d.hits,
		TrafficSeen: d.trafficSeen,
		FirstSeen:   d.first,
		LastSeen:    d.last,
		Tags:        tags,
	}
}

// capSources keeps the first maxSources sources in discovery order and, when
// more existed, appends a "more" marker holding the dropped count so the UI
// can still show "and N more" without bloating every asset row.
func capSources(sources []Source) []Source {
	if len(sources) <= maxSources {
		return sources
	}
	capped := make([]Source, 0, maxSources+1)
	capped = append(capped, sources[:maxSources]...)
	capped = append(capped, Source{Type: "more", Ref: fmt.Sprintf("+%d", len(sources)-maxSources)})
	return capped
}

// collector merges drafts into assets. A "*" method (code or cloud origin)
// acts as a wildcard: traffic with a concrete method folds into the wildcard
// draft of the same scheme|host|path instead of forking a second asset for a
// URL the code already covers. The "*" stays on the merged asset so its ID
// (and the notion "any method applies") remains stable.
type collector struct {
	order      []*draft
	byKey      map[string]*draft
	byWildcard map[string]*draft
}

func newCollector() *collector {
	return &collector{
		byKey:      make(map[string]*draft),
		byWildcard: make(map[string]*draft),
	}
}

func (c *collector) add(d *draft) {
	key := dedupKey(d.method, d.scheme, d.host, d.path)
	if ex := c.byKey[key]; ex != nil {
		ex.merge(d)
		return
	}
	if d.method != "*" {
		if wc := c.byWildcard[dedupKey("*", d.scheme, d.host, d.path)]; wc != nil {
			wc.merge(d)
			return
		}
	}
	c.order = append(c.order, d)
	c.byKey[key] = d
	if d.method == "*" {
		c.byWildcard[dedupKey("*", d.scheme, d.host, d.path)] = d
	}
}

func (c *collector) assets() []Asset {
	out := make([]Asset, 0, len(c.order))
	for _, d := range c.order {
		out = append(out, d.asset())
	}
	return out
}

// ExtractFromCode walks dir (decompiled miniapp output), pulls every
// http(s)/ws(s) URL out of the scannable text files and returns the
// deduplicated assets plus the number of files actually scanned.
func ExtractFromCode(dir string) ([]Asset, int, error) {
	drafts, files, err := collectFromDir(dir)
	if err != nil {
		return nil, 0, err
	}
	c := newCollector()
	for _, d := range drafts {
		c.add(d)
	}
	items := c.assets()
	sortAssets(items)
	return items, files, nil
}

// collectFromDir walks dir and returns one draft per distinct URL key per
// file. Directories named node_modules/.git and files over 2 MB are skipped.
func collectFromDir(dir string) ([]*draft, int, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, 0, err
	}
	if !info.IsDir() {
		return nil, 0, fmt.Errorf("%s 不是目录", dir)
	}
	var drafts []*draft
	files := 0
	walkErr := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, not fatal
		}
		if fi.IsDir() {
			if path != dir && (strings.EqualFold(fi.Name(), "node_modules") || strings.EqualFold(fi.Name(), ".git")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !scannableExt(fi.Name()) || fi.Size() > maxFileRead {
			return nil
		}
		data, err := readFileHead(path, maxFileRead)
		if err != nil {
			return nil
		}
		files++
		drafts = append(drafts, draftsFromContent(data, relativeRef(dir, path))...)
		return nil
	})
	if walkErr != nil {
		return nil, 0, walkErr
	}
	return drafts, files, nil
}

// scannableExt reports whether name is one of the text formats decompiled
// miniapp output uses.
func scannableExt(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".js", ".ts", ".json", ".wxml", ".wxss", ".html", ".map", ".txt":
		return true
	default:
		return false
	}
}

// draftsFromContent extracts every URL occurrence in one file. Repeated
// occurrences of the same URL inside the file bump hits but produce a single
// Source, so an asset shows where it lives without listing a file twice.
func draftsFromContent(content []byte, ref string) []*draft {
	perFile := make(map[string]*draft)
	for _, m := range urlPattern.FindAllString(string(content), -1) {
		p, ok := parseObservedURL(m)
		if !ok {
			continue
		}
		key := dedupKey("*", p.scheme, p.host, p.path)
		d := perFile[key]
		if d == nil {
			d = newDraft(classifyKind(p.scheme, p.path), "*", p)
			perFile[key] = d
		}
		d.addHits(1)
		d.addQueryKeys(p.keys)
	}
	if len(perFile) == 0 {
		return nil
	}
	keys := make([]string, 0, len(perFile))
	for k := range perFile {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*draft, 0, len(perFile))
	for _, k := range keys {
		d := perFile[k]
		d.addSource(Source{Type: "code", Ref: ref})
		out = append(out, d)
	}
	return out
}

// readFileHead reads at most limit bytes of path so a file that grew between
// stat and read cannot balloon memory.
func readFileHead(path string, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, int64(limit)))
}

// relativeRef renders path relative to the scan root with forward slashes;
// a path that escapes the root (symlink) degrades to the base name.
func relativeRef(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return filepath.Base(path)
	}
	return filepath.ToSlash(rel)
}

// ExtractFromRecords turns captured traffic into assets: one asset per
// method|scheme|host|path, hits counting records, sources carrying the
// record IDs. Records without a usable URL are skipped.
func ExtractFromRecords(records []traffic.Record) []Asset {
	c := newCollector()
	for i := range records {
		if d := draftFromRecord(&records[i]); d != nil {
			c.add(d)
		}
	}
	items := c.assets()
	sortAssets(items)
	return items
}

func draftFromRecord(rec *traffic.Record) *draft {
	p, ok := parseObservedURL(rec.URL)
	if !ok {
		return nil
	}
	method := strings.ToUpper(strings.TrimSpace(rec.Method))
	if method == "" {
		method = "GET"
	}
	d := newDraft(classifyKind(p.scheme, p.path), method, p)
	d.addHits(1)
	d.trafficSeen = true
	d.addSource(Source{Type: "traffic", Ref: rec.ID})
	d.observe(rec.CapturedAt)
	d.addQueryKeys(p.keys)
	return d
}
