package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/extract"
	"github.com/langbyyi/wxtap/desktop/internal/rpc"
)

// jsonMarshal is a small indirection so tests can depend on stdlib only.
func jsonMarshal(v any) (string, error) {
	data, err := json.Marshal(v)
	return string(data), err
}

// escapeJS makes a string safe inside an inline single-quoted JS literal.
// Escaping only the quote is insufficient: a backslash can terminate the
// literal, while a raw newline makes the expression invalid.
func escapeJS(s string) string {
	var builder strings.Builder
	builder.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\\':
			builder.WriteString(`\\`)
		case '\'':
			builder.WriteString(`\'`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\u2028':
			builder.WriteString(`\u2028`)
		case '\u2029':
			builder.WriteString(`\u2029`)
		default:
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

// PageConfig is the navigator.pages payload.
type PageConfig struct {
	Pages        []string `json:"pages"`
	TabBarPages  []string `json:"tab_bar_pages"`
	CurrentRoute string   `json:"current_route"`
	AppID        string   `json:"appid"`
	Entry        string   `json:"entry"`
	Name         string   `json:"name"`
}

// PageEntry is one level of the runtime page stack (navigator.pageStack).
type PageEntry struct {
	Route  string            `json:"route"`
	Params map[string]string `json:"params"`
}

// NavigateRequest is the navigator.navigate payload.
type NavigateRequest struct {
	Route  string `json:"route"`
	Method string `json:"method"` // navigateTo | reLaunch | redirectTo | switchTab | navigateBack | refresh
	// Delta is how many pages navigateBack pops (default 1).
	Delta int `json:"delta"`
}

// AutoVisitStep is one auto-visit progress report: how many pages were visited,
// the total, how many failed, and the route the step landed on.
type AutoVisitStep struct {
	Done   int
	Total  int
	Failed int
	Route  string
}

// NavigatorCtl drives the navigator hook for the IPC surface.
type NavigatorCtl struct {
	onProgress func(AutoVisitStep)

	mu       sync.Mutex
	visiting bool
	cancel   context.CancelFunc
	// lastStep is the newest walk progress report; it survives the walk so a
	// poller that arrives after the finish still sees how it went.
	lastStep AutoVisitStep
	// done closes when the walk goroutine unwinds; StopAutoVisit waits on it
	// so a stop followed by an immediate AutoVisitRunning read does not see a
	// stale true while the goroutine is still inside an in-flight evaluate.
	done chan struct{}

	navigator pageReader
}

// pageReader lets the controller reuse navigator.Navigator without an import
// cycle at the app layer; the shell supplies a small adapter.
type pageReader interface {
	FetchConfig(ctx context.Context) (PageConfig, error)
	NavigateTo(ctx context.Context, route string) error
	RelaunchTo(ctx context.Context, route string) error
	RedirectTo(ctx context.Context, route string) error
	SwitchTab(ctx context.Context, route string) error
	NavigateBack(ctx context.Context, delta int) error
	RefreshPage(ctx context.Context) (RefreshResult, error)
	GetCurrentRoute(ctx context.Context) (string, error)
	GetPageStack(ctx context.Context) ([]PageEntry, error)
	GuardOn(ctx context.Context) (bool, error)
	EnableRedirectGuard(ctx context.Context) (GuardResult, error)
	DisableRedirectGuard(ctx context.Context) error
	GetBlockedRedirects(ctx context.Context) ([]map[string]any, error)
	Invalidate()
	Pages() []string
}

// RefreshResult / GuardResult mirror navigator.Navigator's shapes.
type RefreshResult struct {
	OK    bool   `json:"ok"`
	Route string `json:"route"`
	Err   string `json:"err"`
}

type GuardResult struct {
	OK    bool `json:"ok"`
	Count int  `json:"count"`
}

// NewNavigatorCtl wires the controller. onProgress reports one auto-visit step.
func NewNavigatorCtl(navigator pageReader, onProgress func(AutoVisitStep)) *NavigatorCtl {
	return &NavigatorCtl{navigator: navigator, onProgress: onProgress}
}

// Pages fetches config and the current route (navigator.pages shape).
func (c *NavigatorCtl) Pages(ctx context.Context) (PageConfig, error) {
	config, err := c.navigator.FetchConfig(ctx)
	if err != nil {
		return PageConfig{}, err
	}
	route, err := c.navigator.GetCurrentRoute(ctx)
	if err != nil {
		return config, nil // route is best-effort
	}
	config.CurrentRoute = route
	return config, nil
}

// Navigate dispatches by the IPC method field.
func (c *NavigatorCtl) Navigate(ctx context.Context, req NavigateRequest) error {
	// 这三个方法都需要 route：坏 payload（字段名写错等）解出来是空串，页面侧会
	// 变成 goTo('/')——一次莫名其妙的跳转比一个明确的报错糟得多。
	if req.Method != "navigateBack" && req.Method != "refresh" && strings.TrimSpace(req.Route) == "" {
		return fmt.Errorf("缺少 route：%s 必须带目标页面", req.Method)
	}
	switch req.Method {
	case "reLaunch":
		return c.navigator.RelaunchTo(ctx, req.Route)
	case "redirectTo":
		return c.navigator.RedirectTo(ctx, req.Route)
	case "switchTab":
		return c.navigator.SwitchTab(ctx, req.Route)
	case "navigateBack":
		// delta pops that many levels; 1 is the default the UI sends for
		// "back one page", and the runtime rejects a delta it cannot honour.
		delta := req.Delta
		if delta < 1 {
			delta = 1
		}
		return c.navigator.NavigateBack(ctx, delta)
	case "refresh":
		_, err := c.navigator.RefreshPage(ctx)
		return err
	default:
		return c.navigator.NavigateTo(ctx, req.Route)
	}
}

// CurrentRoute returns the current page route.
func (c *NavigatorCtl) CurrentRoute(ctx context.Context) (string, error) {
	return c.navigator.GetCurrentRoute(ctx)
}

// Invalidate drops the page-side injection cache. The navigator hook lives in
// the page realm, so a rebuild removes window.nav while the cache still claims
// it is installed; without this the next navigation call fails against a
// missing window.nav.
func (c *NavigatorCtl) Invalidate() {
	c.navigator.Invalidate()
}

// PageStack returns the runtime page stack wrapped as {stack, current} to
// match the desktop IPC contract.
func (c *NavigatorCtl) PageStack(ctx context.Context) (map[string]any, error) {
	stack, err := c.navigator.GetPageStack(ctx)
	if err != nil {
		return nil, err
	}
	if stack == nil {
		stack = []PageEntry{}
	}
	current := ""
	if len(stack) > 0 {
		current = stack[len(stack)-1].Route
	}
	return map[string]any{"stack": stack, "current": current}, nil
}

// GuardState reports the live redirect-guard state together with its blocked
// log in one round-trip. The guard only exists in the page realm, so reading
// it back is what keeps the toggle honest after a page reload. The blocked log
// is skipped while the guard is off: the page only fills it while intercepting.
//
// With no miniapp connected there is no realm either, so the guard is
// necessarily off — that is reported as a plain disabled state rather than an
// error: a status query ("is the guard on?") that errors reads to a caller as
// "the question is unanswerable", and it is answerable.
func (c *NavigatorCtl) GuardState(ctx context.Context) (map[string]any, error) {
	on, err := c.navigator.GuardOn(ctx)
	if err != nil {
		if coreOffline(err) {
			return map[string]any{"enabled": false, "redirects": []map[string]any{}, "note": "no miniapp connected — the guard lives in the page realm"}, nil
		}
		return nil, err
	}
	if !on {
		return map[string]any{"enabled": false, "redirects": []map[string]any{}}, nil
	}
	blocked, err := c.navigator.GetBlockedRedirects(ctx)
	if err != nil {
		return nil, err
	}
	if blocked == nil {
		blocked = []map[string]any{}
	}
	return map[string]any{"enabled": true, "redirects": blocked}, nil
}

// EnableRedirectGuard / DisableRedirectGuard toggle forced-redirect blocking.
func (c *NavigatorCtl) EnableRedirectGuard(ctx context.Context) (GuardResult, error) {
	return c.navigator.EnableRedirectGuard(ctx)
}

func (c *NavigatorCtl) DisableRedirectGuard(ctx context.Context) error {
	return c.navigator.DisableRedirectGuard(ctx)
}

// GetBlockedRedirects returns the blocked-redirect log wrapped as
// {redirects: [...]} to match the desktop IPC contract. An empty list without
// a target is truthful (nothing can be blocked without a page realm), not an
// error — the accumulate-only-while-enabled rule is documented on the tool.
func (c *NavigatorCtl) GetBlockedRedirects(ctx context.Context) (map[string]any, error) {
	blocked, err := c.navigator.GetBlockedRedirects(ctx)
	if err != nil {
		if coreOffline(err) {
			return map[string]any{"redirects": []map[string]any{}}, nil
		}
		return nil, err
	}
	if blocked == nil {
		blocked = []map[string]any{}
	}
	return map[string]any{"redirects": blocked}, nil
}

// coreOffline reports the no-miniapp / no-engine failures the Core answers
// navigator reads with. Code 1000 is "no miniapp connected"; the message
// check covers the engine-not-started sibling so idle-state queries degrade
// the same way whichever prerequisite is missing.
func coreOffline(err error) bool {
	var coreErr *rpc.CoreError
	if errors.As(err, &coreErr) {
		return coreErr.Code == 1000 || coreErr.Code == 2000
	}
	message := err.Error()
	return strings.Contains(message, "no miniapp connected") || strings.Contains(message, "engine not started")
}

// AutoVisit visits every configured page sequentially in the background and
// reports each step through onProgress. The page list comes from the cached
// config, so an empty cache (the routes were never fetched, or the miniapp was
// just switched) is fetched first — otherwise the walk would silently do
// nothing while reporting a clean finish.
//
// started is false when a walk is already running: the caller must not tell the
// user a new walk began when it merely joined the existing one.
func (c *NavigatorCtl) AutoVisit(ctx context.Context, delay time.Duration) (started bool, err error) {
	c.mu.Lock()
	if c.visiting {
		c.mu.Unlock()
		return false, nil
	}
	// The walk must outlive the request that started it: an MCP/HTTP caller's
	// context is cancelled the moment its response is written, and a walk
	// holding that context would abort after the first page (or, before the
	// rpc layer was fixed, tear the Core connection down with it). Detach from
	// the request; StopAutoVisit's cancel is the only stop path.
	visitCtx, cancel := context.WithCancel(context.Background())
	c.visiting = true
	c.cancel = cancel
	c.done = make(chan struct{})
	c.mu.Unlock()

	pages := c.navigator.Pages()
	if len(pages) == 0 {
		config, err := c.navigator.FetchConfig(visitCtx)
		if err != nil {
			c.finishVisit()
			return false, err
		}
		pages = config.Pages
	}
	if len(pages) == 0 {
		c.finishVisit()
		return false, fmt.Errorf("没有可遍历的页面：小程序尚未上报页面配置")
	}

	go func() {
		defer c.finishVisit()
		failed := 0
		for i, route := range pages {
			if visitCtx.Err() != nil {
				return
			}
			// A page that refuses to load must not abort the walk, but it has
			// to be counted: a fully failing round otherwise reported 100%.
			if err := c.navigator.RelaunchTo(visitCtx, route); err != nil {
				failed++
			}
			step := AutoVisitStep{Done: i + 1, Total: len(pages), Failed: failed, Route: route}
			c.mu.Lock()
			c.lastStep = step
			c.mu.Unlock()
			if c.onProgress != nil {
				c.onProgress(step)
			}
			select {
			case <-visitCtx.Done():
				return
			case <-time.After(delay):
			}
		}
	}()
	return true, nil
}

// AutoVisitRunning reports whether a walk is in flight. The walk lives in the
// shell, so a view that is re-opened must read it back instead of assuming it
// stopped when the page was left.
func (c *NavigatorCtl) AutoVisitRunning() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.visiting
}

// AutoVisitProgress reports the most recent walk step (or zero when no walk
// has run yet). MCP clients cannot receive the progress events, so polling
// this is their only way to tell a crawl in progress from one that finished
// with everything failing.
func (c *NavigatorCtl) AutoVisitProgress() AutoVisitStep {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastStep
}

// finishVisit releases the auto-visit slot. The detached walk context must be
// cancelled here too — natural completion and the early fetch failure both
// pass through — or the context (and everything it anchors) outlives the walk
// for the rest of the process.
func (c *NavigatorCtl) finishVisit() {
	c.mu.Lock()
	c.visiting = false
	if c.cancel != nil {
		c.cancel()
	}
	c.cancel = nil
	if c.done != nil {
		close(c.done)
		c.done = nil
	}
	c.mu.Unlock()
}

// StopAutoVisit cancels a running auto visit and waits for the walk goroutine
// to unwind. Waiting here (without holding mu) keeps the visible state honest:
// without it, the goroutine could still be inside an in-flight evaluate when
// the caller reads AutoVisitRunning and the UI flips back to「遍历中」.
func (c *NavigatorCtl) StopAutoVisit() {
	c.mu.Lock()
	done := c.done
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Unlock()
	if done != nil {
		<-done
	}
}

// ValidateAppID rejects identifiers that would escape their parent directory
// when joined into a path. The rule lives in the extract package so the IPC
// handlers, MCP tools and the report writer cannot drift apart.
func ValidateAppID(appID string) error {
	return extract.ValidateAppID(appID)
}
