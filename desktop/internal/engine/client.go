// Package engine manages the TypeScript Core child process and exposes its
// RPC surface (engine.status / engine.start / engine.stop) to the desktop
// shell. Every failure surfaces as a typed retryable *rpc.CoreError so the
// Wails app can render a recoverable state instead of crashing.
package engine

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"

	"github.com/langbyyi/wxtap/desktop/internal/rpc"
)

// EngineStatus is the Core's engine.status payload.
type EngineStatus struct {
	Frida      bool     `json:"frida"`
	Miniapp    bool     `json:"miniapp"`
	Devtools   bool     `json:"devtools"`
	Generation int64    `json:"generation,omitempty"`
	AppInfo    *AppInfo `json:"appInfo,omitempty"`
}

// AppInfo is the identity the Core probed from a connected miniapp
// (__wxConfig reading: appid + nickname).
type AppInfo struct {
	AppID string `json:"appid"`
	Name  string `json:"name"`
	Icon  string `json:"icon,omitempty"`
}

// MiniappEntry is one connected miniapp in the multi-open list.
type MiniappEntry struct {
	ID     int64  `json:"id"`
	AppID  string `json:"appid"`
	Name   string `json:"name"`
	Locked bool   `json:"locked"`
}

// Client owns one Core process and its RPC connection.
type Client struct {
	cmd *exec.Cmd
	rpc *rpc.Client

	once   sync.Once
	exited chan struct{}

	stderrMu    sync.Mutex
	stderrLines []string
	// pendingLogs holds lines captured before OnLog registered a callback:
	// captureStderr starts inside StartProcess, ahead of the caller's OnLog
	// call, and those first lines are usually the most diagnostic ones.
	pendingLogs []string
	// logMu serialises line delivery. The OnLog replay of pre-registration
	// lines and captureStderr's live deliveries are concurrent paths; without
	// this lock a live line could overtake a replayed one. Callbacks run while
	// holding logMu but never stderrMu.
	logMu sync.Mutex
	onLog func(line string)
}

// maxPendingStderr bounds the pre-registration replay buffer.
const maxPendingStderr = 64

// OnLog registers a callback invoked for every Core stderr line; the desktop
// shell forwards them as `log` events so the control view shows the live
// Core output into the control view log. Lines captured before registration
// are replayed to the callback first, in capture order.
func (c *Client) OnLog(callback func(line string)) {
	c.logMu.Lock()
	c.stderrMu.Lock()
	c.onLog = callback
	pending := c.pendingLogs
	c.pendingLogs = nil
	c.stderrMu.Unlock()
	for _, line := range pending {
		callback(line)
	}
	c.logMu.Unlock()
}

// OnEvent registers a callback for Core-pushed events. The Core pushes console
// records this way: the miniapp's console object is a non-configurable accessor
// on this WMPF build (a page-side hook cannot wrap it), so capture happens at
// the CDP layer and travels as {event:"console"} lines.
func (c *Client) OnEvent(callback func(name string, payload []byte)) {
	c.rpc.OnEvent(func(event rpc.Event) {
		callback(event.Name, event.Payload)
	})
}

// captureStderr keeps the last few stderr lines of the Core so an unexpected
// exit can report its reason instead of just "the process is gone".
func (c *Client) captureStderr(stderr io.ReadCloser) {
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		// logMu keeps this live delivery from overtaking OnLog's replay (see
		// the field comment). The callback may block on the consumer — the
		// scanner pipeline backing up under a burst is acceptable backpressure.
		c.logMu.Lock()
		c.stderrMu.Lock()
		c.stderrLines = append(c.stderrLines, line)
		if len(c.stderrLines) > 32 {
			c.stderrLines = c.stderrLines[len(c.stderrLines)-32:]
		}
		callback := c.onLog
		if callback == nil {
			c.pendingLogs = append(c.pendingLogs, line)
			if len(c.pendingLogs) > maxPendingStderr {
				c.pendingLogs = c.pendingLogs[len(c.pendingLogs)-maxPendingStderr:]
			}
		}
		c.stderrMu.Unlock()
		if callback != nil {
			callback(line)
		}
		c.logMu.Unlock()
	}
}

// RecentStderr returns the last captured stderr lines (oldest first).
func (c *Client) RecentStderr() []string {
	c.stderrMu.Lock()
	defer c.stderrMu.Unlock()
	out := make([]string, len(c.stderrLines))
	copy(out, c.stderrLines)
	return out
}

// StartProcess spawns the Core binary and wires its stdio into an RPC client.
func StartProcess(cmd *exec.Cmd) (*Client, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, offline("cannot open core stdin", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, offline("cannot open core stdout", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, offline("cannot open core stderr", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, offline("cannot start core process", err)
	}

	client := &Client{
		cmd:    cmd,
		rpc:    rpc.NewClient(stdout, stdin),
		exited: make(chan struct{}),
	}
	go client.captureStderr(stderr)
	go func() {
		_ = cmd.Wait()
		close(client.exited)
	}()
	return client, nil
}

// Events exposes Core-pushed events. The Core currently never pushes any
// (status is polled every second); the channel is kept for future use and
// drained lazily.
func (c *Client) Events() <-chan rpc.Event {
	return c.rpc.Events()
}

// Status returns the engine status snapshot.
func (c *Client) Status(ctx context.Context) (EngineStatus, error) {
	var status EngineStatus
	if err := c.call(ctx, "engine.status", map[string]any{}, &status); err != nil {
		return EngineStatus{}, err
	}
	return status, nil
}

// WeChatStatus is the desktop WeChat host as seen before the debug engine attaches.
type WeChatStatus struct {
	Running      bool   `json:"running"`
	PID          int    `json:"pid,omitempty"`
	Version      int    `json:"version,omitempty"`
	Path         string `json:"path,omitempty"`
	AddressTable bool   `json:"addressTable"`
	Note         string `json:"note,omitempty"`
}

// WeChatStatus asks Core which WeChat host is present, including the WMPF build when it can be verified.
func (c *Client) WeChatStatus(ctx context.Context) (WeChatStatus, error) {
	var result WeChatStatus
	if err := c.call(ctx, "wechat.status", map[string]any{}, &result); err != nil {
		return WeChatStatus{}, err
	}
	return result, nil
}

// WeChatRunning reports whether the desktop WeChat process is currently present.
func (c *Client) WeChatRunning(ctx context.Context) (bool, error) {
	status, err := c.WeChatStatus(ctx)
	if err != nil {
		return false, err
	}
	return status.Running, nil
}

// Start injects the Frida hook and brings up the debug/CDP servers.
func (c *Client) Start(ctx context.Context, cdpPort int) error {
	var result struct {
		Frida bool `json:"frida"`
	}
	return c.call(ctx, "engine.start", map[string]any{"cdpPort": cdpPort}, &result)
}

// Stop shuts the engine down gracefully.
func (c *Client) Stop(ctx context.Context) error {
	return c.call(ctx, "engine.stop", map[string]any{}, nil)
}

// HookInstallRecord is one entry of a hook install report.
type HookInstallRecord = map[string]any

// HookDrainedRecord is one buffered hook entry: its sequence and the record.
type HookDrainedRecord struct {
	Seq    int64          `json:"seq"`
	Record map[string]any `json:"record"`
}

// HookDrainedUpdate is one settled-record update frame of a hook's update
// stream: the sequence the consumer acknowledges plus the frame itself
// ({rid, status, result|error, durationMs, settledAt}).
type HookDrainedUpdate struct {
	Seq    int64          `json:"seq"`
	Update map[string]any `json:"update"`
}

// HookDrainPage is the hook.drain response.
type HookDrainPage struct {
	Records       []HookDrainedRecord `json:"records"`
	Updates       []HookDrainedUpdate `json:"updates"`
	NextSeq       int64               `json:"nextSeq"`
	NextUpdateSeq int64               `json:"nextUpdateSeq"`
	HasMore       bool                `json:"hasMore"`
	// DroppedRecords / DroppedUpdates are the page-side buffers' cumulative
	// eviction counts (R12), repeated verbatim on every drain response: they
	// are readings, not cursors, so requesting a page never resets them.
	DroppedRecords int64 `json:"droppedRecords"`
	DroppedUpdates int64 `json:"droppedUpdates"`
}

// HookInstall injects a named hook script into the miniapp runtime.
func (c *Client) HookInstall(ctx context.Context, name string) (HookInstallRecord, error) {
	var report HookInstallRecord
	if err := c.call(ctx, "hook.install", map[string]any{"name": name}, &report); err != nil {
		return nil, err
	}
	return report, nil
}

// HookDrain pulls at most limit records after afterSeq and at most updateLimit
// settled-record updates after afterUpdateSeq from a named hook. Both streams
// share one round trip.
func (c *Client) HookDrain(ctx context.Context, name string, afterSeq int64, limit int, afterUpdateSeq int64, updateLimit int) (HookDrainPage, error) {
	var page HookDrainPage
	params := map[string]any{
		"name":           name,
		"afterSeq":       afterSeq,
		"limit":          limit,
		"afterUpdateSeq": afterUpdateSeq,
		"updateLimit":    updateLimit,
	}
	if err := c.call(ctx, "hook.drain", params, &page); err != nil {
		return HookDrainPage{}, err
	}
	return page, nil
}

// HookClear removes records buffered inside the page-side hook.
func (c *Client) HookClear(ctx context.Context, name string) error {
	return c.call(ctx, "hook.clear", map[string]any{"name": name}, nil)
}

// HookUninstall restores the page APIs wrapped by a named hook.
func (c *Client) HookUninstall(ctx context.Context, name string) error {
	return c.call(ctx, "hook.uninstall", map[string]any{"name": name}, nil)
}

// CloudScan runs the cloud hook's static scanner and returns discovered
// cloud functions, database collections and storage operations.
func (c *Client) CloudScan(ctx context.Context) ([]map[string]any, error) {
	var items []map[string]any
	if err := c.call(ctx, "cloud.scan", map[string]any{}, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// Evaluate runs a JS expression in the miniapp runtime and returns the
// parsed value from Runtime.evaluate (returnByValue).
func (c *Client) Evaluate(ctx context.Context, expression string, timeoutMs int) (any, error) {
	var result struct {
		Value any `json:"value"`
	}
	if err := c.call(ctx, "runtime.evaluate", map[string]any{"expression": expression, "timeoutMs": timeoutMs}, &result); err != nil {
		return nil, err
	}
	return result.Value, nil
}

// EvaluateAwait runs a JS expression that returns a Promise and resolves it
// inside the page (awaitPromise) before returning the settled value.
func (c *Client) EvaluateAwait(ctx context.Context, expression string, timeoutMs int) (any, error) {
	var result struct {
		Value any `json:"value"`
	}
	params := map[string]any{"expression": expression, "timeoutMs": timeoutMs, "awaitPromise": true}
	if err := c.call(ctx, "runtime.evaluate", params, &result); err != nil {
		return nil, err
	}
	return result.Value, nil
}

// MiniappList returns the connected miniapps with their lock state.
func (c *Client) MiniappList(ctx context.Context) ([]MiniappEntry, error) {
	var list []MiniappEntry
	if err := c.call(ctx, "miniapp.list", map[string]any{}, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// MiniappSwitch moves the debug lock to the miniapp with the given id.
func (c *Client) MiniappSwitch(ctx context.Context, id int64) (bool, error) {
	var result struct {
		OK bool `json:"ok"`
	}
	if err := c.call(ctx, "miniapp.switch", map[string]any{"id": id}, &result); err != nil {
		return false, err
	}
	return result.OK, nil
}

// MiniappSetLock toggles the lock switch (off = debug the newest connection).
func (c *Client) MiniappSetLock(ctx context.Context, enabled bool) error {
	return c.call(ctx, "miniapp.setLock", map[string]any{"enabled": enabled}, nil)
}

// MiniappGetLock reports the lock switch state.
func (c *Client) MiniappGetLock(ctx context.Context) (bool, error) {
	var result struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.call(ctx, "miniapp.getLock", map[string]any{}, &result); err != nil {
		return false, err
	}
	return result.Enabled, nil
}

// CDPCommand sends an arbitrary CDP command through the Core bridge and
// returns the raw {id, result} response.
func (c *Client) CDPCommand(ctx context.Context, method string, params map[string]any, timeoutMs int) (map[string]any, error) {
	var result map[string]any
	callParams := map[string]any{"method": method, "params": params, "timeoutMs": timeoutMs}
	if err := c.call(ctx, "cdp.command", callParams, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// DebugState is the Core debugger session snapshot (cdp.debug): whether the
// Debugger domain is enabled, the paused call frames, and the parsed-script
// list. enable=true also sends Debugger.enable so events start flowing.
func (c *Client) DebugState(ctx context.Context, enable bool) (map[string]any, error) {
	var result map[string]any
	if err := c.call(ctx, "cdp.debug", map[string]any{"enable": enable}, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// CodeFormat formats source text through the Core's js-beautify pipeline
// (js-beautify); language "text" passes content through.
func (c *Client) CodeFormat(ctx context.Context, content, language string) (string, error) {
	var result struct {
		Formatted string `json:"formatted"`
	}
	params := map[string]any{"content": content, "language": language}
	if err := c.call(ctx, "code.format", params, &result); err != nil {
		return "", err
	}
	return result.Formatted, nil
}

// Shutdown terminates the Core process and the RPC connection.
func (c *Client) Shutdown() {
	c.once.Do(func() {
		_ = c.rpc.Close()
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		<-c.exited
	})
}

// WaitExited lets the app observe an unexpected Core exit.
func (c *Client) WaitExited() <-chan struct{} {
	return c.exited
}

// Broken reports a connection that was torn down while the process may still
// be alive. writeLine's wedged-writer teardown is the producer: it closes the
// rpc connection on a cancelled (not failed) write, and the Core — its event
// loop pinned by listening servers — keeps running with a dead stdin. ensure
// Engine must treat that as death even though WaitExited never fires.
func (c *Client) Broken() <-chan struct{} {
	return c.rpc.Done()
}

func (c *Client) call(ctx context.Context, method string, params any, result any) error {
	select {
	case <-c.exited:
		return &rpc.CoreError{Code: 1001, Message: "core process has exited", Retryable: true}
	default:
	}
	return c.rpc.Call(ctx, method, params, result)
}

func offline(message string, cause error) error {
	detail := message
	if cause != nil {
		detail = fmt.Sprintf("%s: %v", message, cause)
	}
	return &rpc.CoreError{Code: 1001, Message: detail, Retryable: true}
}
