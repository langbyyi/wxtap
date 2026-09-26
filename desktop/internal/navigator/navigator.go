// Package navigator ports the MiniProgramNavigator: it drives the
// navigator JS (window.nav, injected Core-side as the "navigator" hook)
// through the Core's runtime.evaluate to read page config, navigate pages
// and manage the redirect guard.
package navigator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Core is the subset of the engine client the navigator needs.
type Core interface {
	Evaluate(ctx context.Context, expression string, timeoutMs int) (any, error)
	EvaluateAwait(ctx context.Context, expression string, timeoutMs int) (any, error)
	InstallHook(ctx context.Context, name string) error
}

// navResult is the page-side outcome of one navigation action.
type navResult struct {
	OK    bool   `json:"ok"`
	Err   string `json:"err"`
	Route string `json:"route"`
}

// evaluateNav runs a page-side navigation action and reports what actually
// happened. The page helpers return Promise<{ok, err}>: wx's success/fail are
// callbacks, so awaiting the promise is the only way to know whether the page
// really moved. Without it a failed jump still reported success (and auto-visit
// could never count a failed page).
func (n *Navigator) evaluateNav(ctx context.Context, call string) (navResult, error) {
	value, err := n.core.EvaluateAwait(ctx, "window.nav."+call, evaluateTimeoutMs)
	if err != nil {
		return navResult{}, err
	}
	if value == nil {
		return navResult{}, fmt.Errorf("导航没有返回结果：%s", call)
	}
	var result navResult
	if err := unmarshalValue(value, &result); err != nil {
		return navResult{}, fmt.Errorf("导航结果无法解析：%w", err)
	}
	if !result.OK {
		if result.Err == "" {
			result.Err = "页面未跳转"
		}
		return result, errors.New(result.Err)
	}
	return result, nil
}

// Config is the fetched page configuration.
type Config struct {
	Pages  []string `json:"pages"`
	TabBar []string `json:"tabBar"`
	AppID  string   `json:"appid"`
	Entry  string   `json:"entry"`
	Name   string   `json:"name"`
}

// RefreshResult is the outcome of RefreshPage.
type RefreshResult struct {
	OK    bool   `json:"ok"`
	Route string `json:"route"`
	Err   string `json:"err"`
}

// GuardResult is the redirect-guard toggle outcome.
type GuardResult struct {
	OK    bool `json:"ok"`
	Count int  `json:"count"`
}

// PageEntry is one level of the runtime page stack.
type PageEntry struct {
	Route  string            `json:"route"`
	Params map[string]string `json:"params"`
}

const evaluateTimeoutMs = 5000

// Navigator controls mini program pages through window.nav.
type Navigator struct {
	core  Core
	appID string

	mu       sync.Mutex
	injected bool
	pages    []string
	tabBar   []string
	appInfo  Config
}

// New creates a navigator bound to one miniapp.
func New(core Core, appID string) *Navigator {
	return &Navigator{core: core, appID: appID}
}

// Invalidate drops the injection cache so the next call re-injects
// (used after switching mini programs).
func (n *Navigator) Invalidate() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.injected = false
}

// ensure injects the navigator hook once (or force re-injects).
func (n *Navigator) ensure(ctx context.Context, force bool) error {
	n.mu.Lock()
	need := force || !n.injected
	n.mu.Unlock()
	if !need {
		return nil
	}
	if err := n.core.InstallHook(ctx, "navigator"); err != nil {
		return err
	}
	n.mu.Lock()
	n.injected = true
	n.mu.Unlock()
	return nil
}

// FetchConfig reads pages/tabBar/appid from window.nav (always re-injects so
// a freshly loaded mini program reports its own config).
func (n *Navigator) FetchConfig(ctx context.Context) (Config, error) {
	if err := n.ensure(ctx, true); err != nil {
		return Config{}, err
	}
	value, err := n.core.Evaluate(ctx, "JSON.stringify({pages:window.nav?window.nav.allPages:[],"+
		"tabBar:window.nav?window.nav.tabBarPages:[],"+
		"appid:window.nav&&window.nav.config?(window.nav.config.appid||''):'',"+
		"entry:window.nav&&window.nav.config?(window.nav.config.entryPagePath||''):'',"+
		"name:(function(){try{"+
		"var f=window.nav&&window.nav.wxFrame;"+
		"var a=(f&&f.__wxConfig)||window.__wxConfig||"+
		"(window.wx&&window.wx.__wxConfig)||{};"+
		// accountInfo.nickname is where current WMPF builds put it;
		// accountInfo.appAccount.nickname is the older, nested location.
		"var c=a.accountInfo||{};var b=c.appAccount||{};"+
		"return c.nickname||c.nickName||b.nickname||b.nickName||a.appname||''"+
		"}catch(e){return ''}})()"+
		"})", evaluateTimeoutMs)
	if err != nil {
		return Config{}, err
	}
	var config Config
	if err := unmarshalValue(value, &config); err != nil {
		return Config{}, err
	}
	n.mu.Lock()
	n.pages = config.Pages
	n.tabBar = config.TabBar
	n.appInfo = config
	n.mu.Unlock()
	return config, nil
}

// Pages returns the last fetched page list.
func (n *Navigator) Pages() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string{}, n.pages...)
}

// NavigateTo performs a smart navigate (switchTab for tabbar pages,
// navigateTo otherwise) and fails when the page did not move.
func (n *Navigator) NavigateTo(ctx context.Context, route string) error {
	if err := n.ensure(ctx, false); err != nil {
		return err
	}
	_, err := n.evaluateNav(ctx, fmt.Sprintf("goTo('%s')", quoteJS(route)))
	return err
}

// RedirectTo performs wx.redirectTo via the detected wxFrame.
func (n *Navigator) RedirectTo(ctx context.Context, route string) error {
	if err := n.ensure(ctx, false); err != nil {
		return err
	}
	_, err := n.evaluateNav(ctx, fmt.Sprintf("redirectTo('%s')", quoteJS(route)))
	return err
}

// SwitchTab performs a literal wx.switchTab, for tab bar pages. Unlike
// NavigateTo it never falls back to navigateTo: a requested "switch to this
// tab" that silently became a push would be a different navigation.
func (n *Navigator) SwitchTab(ctx context.Context, route string) error {
	if err := n.ensure(ctx, false); err != nil {
		return err
	}
	_, err := n.evaluateNav(ctx, fmt.Sprintf("switchTo('%s')", quoteJS(route)))
	return err
}

// RelaunchTo uses the safe reLaunch → switchTab → redirectTo fallback chain and
// fails only when every strategy failed.
func (n *Navigator) RelaunchTo(ctx context.Context, route string) error {
	if err := n.ensure(ctx, false); err != nil {
		return err
	}
	_, err := n.evaluateNav(ctx, fmt.Sprintf("_safeNavigate('%s')", quoteJS(route)))
	return err
}

// NavigateBack goes back delta pages.
func (n *Navigator) NavigateBack(ctx context.Context, delta int) error {
	if err := n.ensure(ctx, false); err != nil {
		return err
	}
	_, err := n.evaluateNav(ctx, fmt.Sprintf("back(%d)", delta))
	return err
}

// RefreshPage reLaunches to the current route (with its query string) and
// reports the page's own verdict: the page-side helper awaits wx.reLaunch and
// its redirectTo fallback, so a failed refresh is an error here instead of a
// silent "已刷新".
func (n *Navigator) RefreshPage(ctx context.Context) (RefreshResult, error) {
	if err := n.ensure(ctx, false); err != nil {
		return RefreshResult{}, err
	}
	result, err := n.evaluateNav(ctx, "refreshPage()")
	if err != nil {
		return RefreshResult{OK: false, Route: result.Route, Err: result.Err}, err
	}
	return RefreshResult{OK: true, Route: result.Route}, nil
}

// GetCurrentRoute returns the current page route.
func (n *Navigator) GetCurrentRoute(ctx context.Context) (string, error) {
	if err := n.ensure(ctx, false); err != nil {
		return "", err
	}
	value, err := n.core.Evaluate(ctx, "(function(){try{if(window.nav&&window.nav.wxFrame){"+
		"var p=window.nav.wxFrame.getCurrentPages();"+
		"return p.length?p[p.length-1].route||p[p.length-1].__route__||'':''"+
		"}return ''}catch(e){return ''}})()", 3000)
	if err != nil {
		return "", err
	}
	if s, ok := value.(string); ok {
		return s, nil
	}
	return "", nil
}

// GetPageStack returns the runtime page stack, bottom page first. Unlike
// Pages (the configured page list), this is what the miniapp actually has on
// its navigation stack right now, so it is the only source for "which pages
// can navigateBack reach".
func (n *Navigator) GetPageStack(ctx context.Context) ([]PageEntry, error) {
	if err := n.ensure(ctx, false); err != nil {
		return nil, err
	}
	value, err := n.core.Evaluate(ctx, "JSON.stringify(window.nav.pageStack())", evaluateTimeoutMs)
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, nil
	}
	var stack []PageEntry
	if err := unmarshalValue(value, &stack); err != nil {
		return nil, err
	}
	return stack, nil
}

// GuardOn reports whether the page-side redirect guard is currently
// intercepting navigations. The guard lives in the page realm, so this is the
// only trustworthy reading: a page reload drops it while the toggle in the UI
// still shows it as on.
func (n *Navigator) GuardOn(ctx context.Context) (bool, error) {
	if err := n.ensure(ctx, false); err != nil {
		return false, err
	}
	value, err := n.core.Evaluate(ctx, "window.nav.isRedirectGuardOn()", evaluateTimeoutMs)
	if err != nil {
		return false, err
	}
	on, _ := value.(bool)
	return on, nil
}

// EnableRedirectGuard turns on forced-redirect blocking.
func (n *Navigator) EnableRedirectGuard(ctx context.Context) (GuardResult, error) {
	if err := n.ensure(ctx, false); err != nil {
		return GuardResult{}, err
	}
	value, err := n.core.Evaluate(ctx, "JSON.stringify(window.nav.enableRedirectGuard())", evaluateTimeoutMs)
	if err != nil {
		return GuardResult{}, err
	}
	var result GuardResult
	if value == nil {
		return result, nil
	}
	err = unmarshalValue(value, &result)
	return result, err
}

// DisableRedirectGuard turns off forced-redirect blocking.
func (n *Navigator) DisableRedirectGuard(ctx context.Context) error {
	if err := n.ensure(ctx, false); err != nil {
		return err
	}
	_, err := n.core.Evaluate(ctx, "window.nav.disableRedirectGuard()", evaluateTimeoutMs)
	return err
}

// GetBlockedRedirects returns the blocked-redirect log.
func (n *Navigator) GetBlockedRedirects(ctx context.Context) ([]map[string]any, error) {
	if err := n.ensure(ctx, false); err != nil {
		return nil, err
	}
	value, err := n.core.Evaluate(ctx, "JSON.stringify(window.nav.getBlockedRedirects())", evaluateTimeoutMs)
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, nil
	}
	var blocked []map[string]any
	err = unmarshalValue(value, &blocked)
	return blocked, err
}

// quoteJS makes a route safe inside an inline single-quoted JS literal.
// Escaping only the quote is insufficient: a backslash can terminate the
// literal, while a raw newline makes the expression invalid.
func quoteJS(route string) string {
	var builder strings.Builder
	builder.Grow(len(route))
	for _, r := range route {
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

// unmarshalValue converts a Runtime.evaluate value (already parsed by the
// Core) into the target type, handling string-wrapped JSON.
func unmarshalValue(value any, target any) error {
	switch v := value.(type) {
	case string:
		return json.Unmarshal([]byte(v), target)
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, target)
	}
}
