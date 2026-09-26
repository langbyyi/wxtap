package ipc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/rpc"
)

// stubPageReader records what the controller asked of it. The auto-visit walk
// runs on a background goroutine while the test polls those recordings from its
// own, so every field it touches is behind the mutex: `go test -race` is what
// caught that, and the accessors below are the only sanctioned way to read it.
type stubPageReader struct {
	mu      sync.Mutex
	blocked []map[string]any
	err     error
	stack   []PageEntry
	guardOn bool
	// cached is what Pages() reports (the navigator's cached config page
	// list); pages is what FetchConfig returns.
	cached      []string
	pages       []string
	fetched     []string
	relaunched  []string
	relaunchErr map[string]error
	switched    []string
	backDeltas  []int
}

func (s *stubPageReader) FetchConfig(context.Context) (PageConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetched = append(s.fetched, "fetch")
	return PageConfig{Pages: s.pages}, s.err
}
func (s *stubPageReader) NavigateTo(context.Context, string) error { return nil }
func (s *stubPageReader) RelaunchTo(_ context.Context, route string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.relaunched = append(s.relaunched, route)
	if s.relaunchErr != nil {
		return s.relaunchErr[route]
	}
	return nil
}
func (s *stubPageReader) RedirectTo(context.Context, string) error { return nil }
func (s *stubPageReader) SwitchTab(_ context.Context, route string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.switched = append(s.switched, route)
	return nil
}
func (s *stubPageReader) NavigateBack(_ context.Context, delta int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backDeltas = append(s.backDeltas, delta)
	return nil
}
func (s *stubPageReader) RefreshPage(context.Context) (RefreshResult, error) {
	return RefreshResult{}, nil
}
func (s *stubPageReader) GetCurrentRoute(context.Context) (string, error) { return "", nil }
func (s *stubPageReader) GetPageStack(context.Context) ([]PageEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stack, s.err
}
func (s *stubPageReader) GuardOn(context.Context) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.guardOn, s.err
}
func (s *stubPageReader) EnableRedirectGuard(context.Context) (GuardResult, error) {
	return GuardResult{}, nil
}
func (s *stubPageReader) DisableRedirectGuard(context.Context) error { return nil }
func (s *stubPageReader) GetBlockedRedirects(context.Context) ([]map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blocked, s.err
}
func (s *stubPageReader) Invalidate() {}
func (s *stubPageReader) Pages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cached
}

// walked returns a copy of the routes RelaunchTo was called with.
func (s *stubPageReader) walked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.relaunched...)
}

// fetchCount reports how often the config was fetched.
func (s *stubPageReader) fetchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.fetched)
}

// deltas returns a copy of the navigateBack arguments.
func (s *stubPageReader) deltas() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int{}, s.backDeltas...)
}

func (s *stubPageReader) switchedRoutes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.switched...)
}

func TestGetBlockedRedirectsWrapsIPCShape(t *testing.T) {
	ctl := NewNavigatorCtl(&stubPageReader{blocked: []map[string]any{{"url": "/x"}}}, nil)
	got, err := ctl.GetBlockedRedirects(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	redirects, ok := got["redirects"].([]map[string]any)
	if !ok || len(redirects) != 1 || redirects[0]["url"] != "/x" {
		t.Fatalf("got %#v", got)
	}
}

func TestGetBlockedRedirectsNilBecomesEmpty(t *testing.T) {
	ctl := NewNavigatorCtl(&stubPageReader{}, nil)
	got, err := ctl.GetBlockedRedirects(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	redirects, ok := got["redirects"].([]map[string]any)
	if !ok || redirects == nil || len(redirects) != 0 {
		t.Fatalf("got %#v", got)
	}
}

// The stack is the runtime truth for "which pages can navigateBack reach", so
// the payload carries both the levels and the route the UI highlights.
func TestPageStackReportsLevelsAndCurrentRoute(t *testing.T) {
	reader := &stubPageReader{stack: []PageEntry{
		{Route: "pages/index", Params: map[string]string{"from": "tab"}},
		{Route: "pages/detail", Params: map[string]string{}},
	}}
	ctl := NewNavigatorCtl(reader, nil)

	got, err := ctl.PageStack(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	stack, ok := got["stack"].([]PageEntry)
	if !ok || len(stack) != 2 || stack[1].Route != "pages/detail" {
		t.Fatalf("stack: %#v", got["stack"])
	}
	if got["current"] != "pages/detail" {
		t.Fatalf("current: %#v", got["current"])
	}
}

func TestPageStackEmptyStaysAnEmptyArray(t *testing.T) {
	ctl := NewNavigatorCtl(&stubPageReader{}, nil)

	got, err := ctl.PageStack(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	stack, ok := got["stack"].([]PageEntry)
	if !ok || stack == nil {
		// A null payload breaks clients that iterate the stack.
		t.Fatalf("empty stack must marshal as []: %#v", got["stack"])
	}
	if got["current"] != "" {
		t.Fatalf("current: %#v", got["current"])
	}
}

// The guard only exists in the page realm, so reading it back is what keeps
// the toggle honest after a page reload dropped it.
func TestGuardStateSkipsTheBlockedLogWhileTheGuardIsOff(t *testing.T) {
	reader := &stubPageReader{guardOn: false, blocked: []map[string]any{{"url": "/stale"}}}
	ctl := NewNavigatorCtl(reader, nil)

	got, err := ctl.GuardState(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got["enabled"] != false {
		t.Fatalf("enabled: %#v", got["enabled"])
	}
	redirects, ok := got["redirects"].([]map[string]any)
	if !ok || len(redirects) != 0 {
		t.Fatalf("a disabled guard has no live blocked log: %#v", got["redirects"])
	}
}

func TestGuardStateReturnsTheBlockedLogWhileTheGuardIsOn(t *testing.T) {
	reader := &stubPageReader{guardOn: true, blocked: []map[string]any{{"url": "/blocked"}}}
	ctl := NewNavigatorCtl(reader, nil)

	got, err := ctl.GuardState(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got["enabled"] != true {
		t.Fatalf("enabled: %#v", got["enabled"])
	}
	redirects, ok := got["redirects"].([]map[string]any)
	if !ok || len(redirects) != 1 || redirects[0]["url"] != "/blocked" {
		t.Fatalf("redirects: %#v", got["redirects"])
	}
}

// 没有连接目标时守卫必然是关的（它活在小程序页面域里）：状态查询要如实回答
// 「关着」，而不是把「没有目标」当成「问题无法回答」抛出去。
func TestGuardStateWithoutATargetReportsDisabled(t *testing.T) {
	reader := &stubPageReader{err: &rpc.CoreError{Code: 1000, Message: "no miniapp connected", Retryable: true}}
	ctl := NewNavigatorCtl(reader, nil)

	state, err := ctl.GuardState(context.Background())
	if err != nil {
		t.Fatalf("无目标时状态查询不应报错: %v", err)
	}
	if state["enabled"] != false {
		t.Fatalf("enabled: %#v", state["enabled"])
	}
	redirects, err := ctl.GetBlockedRedirects(context.Background())
	if err != nil {
		t.Fatalf("无目标时读拦截清单不应报错: %v", err)
	}
	if blocked, ok := redirects["redirects"].([]map[string]any); !ok || len(blocked) != 0 {
		t.Fatalf("无目标时的拦截清单应为空: %#v", redirects["redirects"])
	}
}

// 遍历必须活得比发起它的请求久：MCP/HTTP 调用的 context 在应答写出后即被
// 取消，而遍历要继续走完全部路由——取消发起请求的 context 不得中止遍历。
func TestAutoVisitWalkSurvivesCallerCancellation(t *testing.T) {
	reader := &stubPageReader{pages: []string{"pages/a", "pages/b", "pages/c"}}
	ctl := NewNavigatorCtl(reader, nil)

	ctx, cancel := context.WithCancel(context.Background())
	started, err := ctl.AutoVisit(ctx, time.Millisecond)
	if err != nil || !started {
		t.Fatalf("started=%v err=%v", started, err)
	}
	cancel() // 应答已写出、调用方已离开——遍历不许跟着死。

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := reader.walked(); len(got) == 3 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("取消请求 context 后遍历中止: walked=%v", reader.walked())
}

// 进度要能被晚到的轮询者看到：state 的 progress 是 MCP 客户端唯一的进度
// 来源，走完之后也应能读到最终计数。
func TestAutoVisitProgressSurvivesTheWalk(t *testing.T) {
	reader := &stubPageReader{pages: []string{"pages/a", "pages/b"}}
	ctl := NewNavigatorCtl(reader, nil)

	started, err := ctl.AutoVisit(context.Background(), time.Millisecond)
	if err != nil || !started {
		t.Fatalf("started=%v err=%v", started, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(reader.walked()) == 2 && !ctl.AutoVisitRunning() {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if step := ctl.AutoVisitProgress(); step.Done != 2 || step.Total != 2 {
		t.Fatalf("progress = %+v, want done=2 total=2", ctl.AutoVisitProgress())
	}
}

func TestNavigateBackUsesTheRequestedDelta(t *testing.T) {
	reader := &stubPageReader{}
	ctl := NewNavigatorCtl(reader, nil)

	if err := ctl.Navigate(context.Background(), NavigateRequest{Method: "navigateBack", Delta: 3}); err != nil {
		t.Fatalf("err: %v", err)
	}
	// A missing or nonsense delta must still mean "back one page".
	if err := ctl.Navigate(context.Background(), NavigateRequest{Method: "navigateBack"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := ctl.Navigate(context.Background(), NavigateRequest{Method: "navigateBack", Delta: -2}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := reader.deltas(); len(got) != 3 || got[0] != 3 || got[1] != 1 || got[2] != 1 {
		t.Fatalf("deltas: %#v", got)
	}
}

// switchTab 必须落到真正的 switchTab：以前它没有 case，落进 default 变成智能
// goTo —— 目标不是 tab 页时，「切到这个 tab」就静默成了一次 push。
func TestNavigateDispatchesSwitchTab(t *testing.T) {
	reader := &stubPageReader{}
	ctl := NewNavigatorCtl(reader, nil)

	if err := ctl.Navigate(context.Background(), NavigateRequest{Method: "switchTab", Route: "pages/index/index"}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if got := reader.switchedRoutes(); len(got) != 1 || got[0] != "pages/index/index" {
		t.Fatalf("switchTab 未派发到 SwitchTab: %#v", got)
	}
	// 与 navigateTo 一致：缺 route 的 switchTab 是坏 payload，必须报错而不是
	// 让页面侧去 goTo('/')。
	if err := ctl.Navigate(context.Background(), NavigateRequest{Method: "switchTab"}); err == nil {
		t.Fatal("switchTab 缺 route 必须报错")
	}
}

// Auto-visit used to walk the cached page list only: with an empty cache it
// reported a clean 100% while never navigating anywhere.
func TestAutoVisitFetchesThePageListWhenTheCacheIsEmpty(t *testing.T) {
	reader := &stubPageReader{pages: []string{"pages/a", "pages/b"}}
	ctl := NewNavigatorCtl(reader, nil)

	if started, err := ctl.AutoVisit(context.Background(), 0); err != nil || !started {
		t.Fatalf("AutoVisit: started=%v err=%v", started, err)
	}
	waitFor(t, func() bool { return len(reader.walked()) == 2 })
	if reader.fetchCount() == 0 {
		t.Fatal("an empty page cache must be fetched before walking")
	}
	if walked := reader.walked(); walked[0] != "pages/a" || walked[1] != "pages/b" {
		t.Fatalf("walked %#v", walked)
	}
}

func TestAutoVisitRefusesToStartWithoutAnyPages(t *testing.T) {
	reader := &stubPageReader{}
	ctl := NewNavigatorCtl(reader, nil)

	started, err := ctl.AutoVisit(context.Background(), 0)
	if err == nil {
		t.Fatal("a mini program without pages cannot be walked")
	}
	if started {
		t.Fatal("a refused walk must not report itself as started")
	}
	if walked := reader.walked(); len(walked) != 0 {
		t.Fatalf("nothing to walk: %#v", walked)
	}
}

// A page that will not load must not stop the walk, but it has to be counted:
// a fully failing round otherwise reported success.
func TestAutoVisitCountsFailedSteps(t *testing.T) {
	reader := &stubPageReader{
		cached:      []string{"pages/a", "pages/b"},
		relaunchErr: map[string]error{"pages/a": errors.New("reLaunch failed")},
	}
	var mu sync.Mutex
	var progress []AutoVisitStep
	ctl := NewNavigatorCtl(reader, func(step AutoVisitStep) {
		mu.Lock()
		defer mu.Unlock()
		progress = append(progress, step)
	})

	if _, err := ctl.AutoVisit(context.Background(), 0); err != nil {
		t.Fatalf("AutoVisit: %v", err)
	}
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(progress) == 2
	})
	mu.Lock()
	defer mu.Unlock()
	if progress[0].Done != 1 || progress[0].Failed != 1 {
		t.Fatalf("first step: %#v", progress[0])
	}
	// The final step carries the complete count, so the UI never shows a
	// failing walk as a clean 100%.
	last := progress[len(progress)-1]
	if last.Done != 2 || last.Total != 2 || last.Failed != 1 || last.Route != "pages/b" {
		t.Fatalf("last step: %#v", last)
	}
}

func TestAutoVisitIgnoresASecondStartWhileVisiting(t *testing.T) {
	reader := &stubPageReader{cached: []string{"pages/a", "pages/b"}}
	ctl := NewNavigatorCtl(reader, nil)

	if started, err := ctl.AutoVisit(context.Background(), time.Hour); err != nil || !started {
		t.Fatalf("AutoVisit: started=%v err=%v", started, err)
	}
	// The first walk is parked on its delay after one step.
	waitFor(t, func() bool { return len(reader.walked()) == 1 })
	// 第二轮不该被当成「新开始」：界面据此提示「已在遍历中」而不是谎报开始。
	secondStarted, err := ctl.AutoVisit(context.Background(), 0)
	if err != nil || secondStarted {
		t.Fatalf("a second start must report started=false: started=%v err=%v", secondStarted, err)
	}
	if !ctl.AutoVisitRunning() {
		t.Fatal("the running walk must be readable while it is in flight")
	}
	ctl.StopAutoVisit()
	// Give a second walk every chance to appear before declaring it absent.
	time.Sleep(50 * time.Millisecond)
	if walked := reader.walked(); len(walked) != 1 {
		t.Fatalf("a second start must not queue a parallel walk: %#v", walked)
	}
}

// blockingRelaunchReader parks every reLaunch on its context, the way a real
// evaluate sits on an in-flight RPC until cancellation unwinds it.
type blockingRelaunchReader struct {
	*stubPageReader
}

func (b *blockingRelaunchReader) RelaunchTo(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

// Stopping must not return before the walk goroutine cleared visiting:
// NavigatorView stops and immediately reads the state back, and a stale true
// would flip the UI back to「遍历中」after the user pressed stop.
func TestStopAutoVisitWaitsForTheWalkToUnwind(t *testing.T) {
	reader := &blockingRelaunchReader{stubPageReader: &stubPageReader{cached: []string{"pages/a", "pages/b"}}}
	ctl := NewNavigatorCtl(reader, nil)

	if started, err := ctl.AutoVisit(context.Background(), 0); err != nil || !started {
		t.Fatalf("AutoVisit: started=%v err=%v", started, err)
	}
	// The first reLaunch is in flight here: StopAutoVisit returns only after
	// the goroutine observed the cancellation.
	ctl.StopAutoVisit()
	if ctl.AutoVisitRunning() {
		t.Fatal("a stopped walk must not report itself as running")
	}
	// The slot is free at once: a new walk starts instead of being refused.
	if started, err := ctl.AutoVisit(context.Background(), 0); err != nil || !started {
		t.Fatalf("restart: started=%v err=%v", started, err)
	}
	ctl.StopAutoVisit()
}

func waitFor(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the background walk")
}

// ValidateAppID is the shared guard for every path built from an appid (IPC
// handlers and MCP tools alike).
func TestValidateAppID(t *testing.T) {
	for _, bad := range []string{"", " ", "..", ".", "../evil", "a/b", `a\b`, " wx123", "wx123 "} {
		if err := ValidateAppID(bad); err == nil {
			t.Fatalf("appid %q must be rejected", bad)
		}
	}
	for _, ok := range []string{"wxone", "wx1234567890abcdef", "ww1234567890abcdef"} {
		if err := ValidateAppID(ok); err != nil {
			t.Fatalf("appid %q should be accepted: %v", ok, err)
		}
	}
}

// 空 route 会退化成 goTo('/')：坏 payload 必须被拒绝，而不是变成一次莫名其妙的跳转。
func TestNavigateRejectsAnEmptyRoute(t *testing.T) {
	reader := &stubPageReader{cached: []string{"pages/a"}}
	ctl := NewNavigatorCtl(reader, nil)
	ctx := context.Background()

	for _, method := range []string{"navigateTo", "reLaunch", "redirectTo", ""} {
		if err := ctl.Navigate(ctx, NavigateRequest{Method: method, Route: "  "}); err == nil {
			t.Fatalf("%q with a blank route must be rejected", method)
		}
	}
	// navigateBack / refresh 不需要 route。
	if err := ctl.Navigate(ctx, NavigateRequest{Method: "navigateBack", Delta: 2}); err != nil {
		t.Fatalf("navigateBack must not require a route: %v", err)
	}
}

func TestAutoVisitRunningIsFalseAfterTheWalkStops(t *testing.T) {
	reader := &stubPageReader{cached: []string{"pages/a", "pages/b"}}
	ctl := NewNavigatorCtl(reader, nil)

	if _, err := ctl.AutoVisit(context.Background(), time.Hour); err != nil {
		t.Fatalf("AutoVisit: %v", err)
	}
	ctl.StopAutoVisit()
	waitFor(t, func() bool { return !ctl.AutoVisitRunning() })
}
