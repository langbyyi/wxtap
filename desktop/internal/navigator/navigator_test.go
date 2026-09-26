package navigator

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// fakeCore records evaluate expressions and answers with canned values.
type fakeCore struct {
	mu        sync.Mutex
	exprs     []string
	responses []string // JSON values returned in rotation
	installed []string
}

func (f *fakeCore) Evaluate(_ context.Context, expression string, _ int) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exprs = append(f.exprs, expression)
	if len(f.responses) == 0 {
		return nil, nil
	}
	var value any
	_ = json.Unmarshal([]byte(f.responses[0]), &value)
	f.responses = f.responses[1:]
	return value, nil
}

// EvaluateAwait mirrors Evaluate for the tests: the navigator uses it to read
// the settled Promise<{ok, err}> that the page-side navigation helpers return.
func (f *fakeCore) EvaluateAwait(ctx context.Context, expression string, timeoutMs int) (any, error) {
	return f.Evaluate(ctx, expression, timeoutMs)
}

func (f *fakeCore) InstallHook(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installed = append(f.installed, name)
	return nil
}

func (f *fakeCore) expressions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.exprs...)
}

func TestFetchConfigParsesNavConfig(t *testing.T) {
	core := &fakeCore{responses: []string{`{
		"pages": ["pages/index/index", "pages/log/log"],
		"tabBar": ["pages/index/index"],
		"appid": "wx123",
		"entry": "pages/index/index",
		"name": "Demo"
	}`}}
	nav := New(core, "wx123")

	config, err := nav.FetchConfig(context.Background())
	if err != nil {
		t.Fatalf("fetch config: %v", err)
	}
	if len(config.Pages) != 2 || config.Pages[0] != "pages/index/index" {
		t.Fatalf("pages not parsed: %+v", config)
	}
	if config.AppID != "wx123" || config.Entry != "pages/index/index" || config.Name != "Demo" {
		t.Fatalf("app info not parsed: %+v", config)
	}
	if len(core.installed) != 1 || core.installed[0] != "navigator" {
		t.Fatalf("navigator hook not installed once: %v", core.installed)
	}
}

func TestNavigateToQuotesRoute(t *testing.T) {
	core := &fakeCore{responses: []string{`{"ok":true}`}}
	nav := New(core, "wx123")

	if err := nav.NavigateTo(context.Background(), "pages/detail/index?id=1"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	exprs := core.expressions()
	if len(exprs) != 1 || !strings.Contains(exprs[0], `window.nav.goTo('pages/detail/index?id=1')`) {
		t.Fatalf("unexpected expression: %v", exprs)
	}
}

// SwitchTab 走页面侧的 switchTo，而不是复用智能 goTo：调用方明确要求切 tab，
// 目标不是 tab 页时静默变成 push 是另一次导航。
func TestSwitchTabUsesThePagesOwnSwitchTo(t *testing.T) {
	core := &fakeCore{responses: []string{`{"ok":true}`}}
	nav := New(core, "wx123")

	if err := nav.SwitchTab(context.Background(), "pages/index/index"); err != nil {
		t.Fatalf("switchTab: %v", err)
	}
	exprs := core.expressions()
	if len(exprs) != 1 || !strings.Contains(exprs[0], `window.nav.switchTo('pages/index/index')`) {
		t.Fatalf("unexpected expression: %v", exprs)
	}
}

// 页面说没跳成，Go 就必须报错：以前失败被吞掉，界面提示「已跳转」，自动遍历
// 也永远计不到失败页。
func TestNavigationFailuresSurfaceThePageVerdict(t *testing.T) {
	core := &fakeCore{responses: []string{
		`{"ok":false,"err":"reLaunch/switchTab/redirectTo 全部失败：navigateTo:fail"}`,
		`{"ok":false,"err":"navigateBack:fail"}`,
		`{"ok":true}`,
	}}
	nav := New(core, "wx123")
	ctx := context.Background()

	if err := nav.RelaunchTo(ctx, "pages/nowhere"); err == nil || !strings.Contains(err.Error(), "全部失败") {
		t.Fatalf("relaunch failure must surface: %v", err)
	}
	if err := nav.NavigateBack(ctx, 1); err == nil || !strings.Contains(err.Error(), "navigateBack") {
		t.Fatalf("navigateBack failure must surface: %v", err)
	}
	if err := nav.NavigateTo(ctx, "pages/index/index"); err != nil {
		t.Fatalf("successful navigation must not error: %v", err)
	}
}

func TestNavigateToRejectsAnEmptyPageVerdict(t *testing.T) {
	// 页面返回空（例如 nav 被换掉）时不能当成功：没有 ok 字段就是没跳成。
	core := &fakeCore{responses: []string{`{}`}}
	nav := New(core, "wx123")

	if err := nav.NavigateTo(context.Background(), "pages/index/index"); err == nil {
		t.Fatal("a verdict without ok must not count as success")
	}
}

func TestEnsureInjectsOnlyOnce(t *testing.T) {
	core := &fakeCore{responses: []string{`""`, `""`}}
	nav := New(core, "wx123")

	_, _ = nav.GetCurrentRoute(context.Background())
	_, _ = nav.GetCurrentRoute(context.Background())
	if len(core.installed) != 1 {
		t.Fatalf("injection repeated: %v", core.installed)
	}

	nav.Invalidate()
	_, _ = nav.GetCurrentRoute(context.Background())
	if len(core.installed) != 2 {
		t.Fatalf("force re-injection after invalidate missing: %v", core.installed)
	}
}

func TestRefreshPageAndGuard(t *testing.T) {
	core := &fakeCore{responses: []string{`{"ok":true,"route":"pages/index"}`, `{"ok":true}`, `[{"url":"/x"}]`}}
	nav := New(core, "wx123")

	refresh, err := nav.RefreshPage(context.Background())
	if err != nil || !refresh.OK || refresh.Route != "pages/index" {
		t.Fatalf("refresh: %v %+v", err, refresh)
	}
	guard, err := nav.EnableRedirectGuard(context.Background())
	if err != nil || !guard.OK {
		t.Fatalf("guard: %v %+v", err, guard)
	}
	blocked, err := nav.GetBlockedRedirects(context.Background())
	if err != nil || len(blocked) != 1 || blocked[0]["url"] != "/x" {
		t.Fatalf("blocked: %v %+v", err, blocked)
	}
}

// The navigation entry points emit exact JS expressions that the injected
// nav.js understands; a refactor that changes the shape silently breaks
// navigation, so the expressions are pinned along with route quoting.
func TestNavigationExpressionsMatchInjectedHookAPI(t *testing.T) {
	core := &fakeCore{responses: []string{
		`{"pages":["pages/index/index"]}`,
		`{"ok":true}`,
		`{"ok":true}`,
		`{"ok":true}`,
	}}
	nav := New(core, "wx123")
	ctx := context.Background()
	_, _ = nav.FetchConfig(ctx) // installs the hook and fills the page cache

	if err := nav.RedirectTo(ctx, "pages/a/a"); err != nil {
		t.Fatalf("redirectTo: %v", err)
	}
	if err := nav.RelaunchTo(ctx, "pages/b/b"); err != nil {
		t.Fatalf("relaunchTo: %v", err)
	}
	if err := nav.NavigateBack(ctx, 2); err != nil {
		t.Fatalf("navigateBack: %v", err)
	}
	if err := nav.DisableRedirectGuard(ctx); err != nil {
		t.Fatalf("disable guard: %v", err)
	}

	exprs := core.expressions()
	joined := strings.Join(exprs, "\n")
	for _, want := range []string{
		`window.nav.redirectTo('pages/a/a')`,
		`window.nav._safeNavigate('pages/b/b')`,
		"window.nav.back(2)",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("navigation expression missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "redirectTo('//") {
		t.Fatalf("route must not be double-slashed:\n%s", joined)
	}
}

// Pages returns a copy: callers must not be able to mutate the cache.
func TestPagesReturnsCopy(t *testing.T) {
	core := &fakeCore{responses: []string{`{"pages":["pages/index/index","pages/log/log"]}`}}
	nav := New(core, "wx123")
	if _, err := nav.FetchConfig(context.Background()); err != nil {
		t.Fatalf("fetch config: %v", err)
	}

	pages := nav.Pages()
	if len(pages) != 2 {
		t.Fatalf("pages: %#v", pages)
	}
	pages[0] = "mutated"
	if got := nav.Pages()[0]; got != "pages/index/index" {
		t.Fatalf("caller mutated the navigator cache: %q", got)
	}
}

func TestNavigateEscapesControlCharacters(t *testing.T) {
	core := &fakeCore{responses: []string{`{"ok":true}`}}
	nav := New(core, "wx123")
	if err := nav.NavigateTo(context.Background(), "a\\b\n'c"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	exprs := core.expressions()
	if len(exprs) != 1 || strings.Contains(exprs[0], "\n'c") || !strings.Contains(exprs[0], `a\\b\n\'c`) {
		t.Fatalf("route control characters were not escaped: %v", exprs)
	}
}
