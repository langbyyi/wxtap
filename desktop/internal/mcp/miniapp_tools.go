package mcp

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/langbyyi/wxtap/desktop/internal/api/ipc"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type imageResult struct {
	Data     string
	MimeType string
	// Note is an optional second content block (text) riding with the image —
	// the route/viewport metadata a screenshot needs to self-check which page
	// it actually captured.
	Note string
}

func (s *Server) callMiniappTool(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	args := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, fmt.Errorf("invalid tool arguments: %w", err)
		}
	}
	switch name {
	case "miniapp_screenshot":
		format := stringArg(args, "format", "png")
		params := map[string]any{"format": format}
		if format == "jpeg" {
			params["quality"] = intArg(args, "quality", 80)
		}
		resp, err := s.cdp(ctx, "Page.captureScreenshot", params, 10000)
		if err != nil {
			return nil, err
		}
		data, _ := nested(resp, "result", "data").(string)
		if data == "" {
			return nil, fmt.Errorf("screenshot failed - no data returned")
		}
		mime := "image/png"
		if format == "jpeg" {
			mime = "image/jpeg"
		}
		return imageResult{Data: data, MimeType: mime, Note: s.screenshotNote(ctx)}, nil
	case "miniapp_click":
		if nodePath := stringArg(args, "node_path", ""); nodePath != "" {
			// a11y 树的路径寻址点击：按兄弟序号路径重定位（0/2/1 = body 下第 0
			// 个孩子的第 2 个孩子的第 1 个）。组件桥可达时发 exparser tap——坐标
			// 点击落在 web-view 下面、虚拟节点吃不到 DOM click 的场景实测都存在。
			contextID, err := s.pageContextID(ctx)
			if err != nil {
				return nil, err
			}
			expression := `(function(){var el=document.body;var parts=` + jsLiteral(nodePath) + `.split('/');for(var i=0;i<parts.length;i++){if(parts[i]===''){continue}var idx=parseInt(parts[i],10);if(isNaN(idx)||!el.children||idx>=el.children.length)return JSON.stringify({error:'Node not found: '+` + jsLiteral(nodePath) + `});el=el.children[idx]}var tag=(el.tagName||'').toLowerCase();var r=el.getBoundingClientRect();var via='dom';try{var wxel=el.__wxElement;if(wxel&&window.exparser&&typeof window.exparser.triggerEvent==='function'){window.exparser.triggerEvent(wxel,'tap',{});via='exparser'}}catch(e){via='dom'}if(via==='dom')el.click();return JSON.stringify({ok:true,x:Math.round(r.left+r.width/2),y:Math.round(r.top+r.height/2),tag:tag,via:via})})()`
			return inlineErrorResult(s.evaluateJSON(ctx, expression, false, contextID, 10000))
		}
		if selector := stringArg(args, "selector", ""); selector != "" {
			// 小程序的 DOM 只存在于当前页 webview。宿主页里根本没有小程序元素，
			// 在默认上下文里 querySelector 只会回「Element not found」。
			contextID, err := s.pageContextID(ctx)
			if err != nil {
				return nil, err
			}
			expression := `(function(){var el=document.querySelector(` + jsLiteral(selector) + `);if(!el)return JSON.stringify({error:'Element not found'});var r=el.getBoundingClientRect();var via='dom';try{var wxel=el.__wxElement;if(wxel&&window.exparser&&typeof window.exparser.triggerEvent==='function'){window.exparser.triggerEvent(wxel,'tap',{});via='exparser'}}catch(e){via='dom'}if(via==='dom')el.click();return JSON.stringify({ok:true,x:Math.round(r.left+r.width/2),y:Math.round(r.top+r.height/2),tag:(el.tagName||'').toLowerCase(),via:via})})()`
			// 没点到必须读作失败：选择器命中不了元素时页内回 {error}，而按
			// isError 判断的 agent 会把带 error 字段的成功当成点过了。
			return inlineErrorResult(s.evaluateJSON(ctx, expression, false, contextID, 10000))
		}
		x, xOK := numberArg(args, "x")
		y, yOK := numberArg(args, "y")
		if !xOK || !yOK {
			// 参数不全是一次失败，口径与其余工具的参数校验一致（那些都回
			// 「missing required argument」/「… must be one of …」的工具错误）。
			return nil, errors.New("provide either selector or both x and y coordinates")
		}
		// 坐标是页面 CSS 像素（典型视口 414x780），不是截图像素——截图按
		// devicePixelRatio 放大（1.5x 时宽约 621）。落点在视口外必然什么都没
		// 点到，静默 ok:true 会把 agent 引去怀疑页面而不是坐标。
		viewport, vpErr := s.cdp(ctx, "Page.getLayoutMetrics", map[string]any{}, 5000)
		if vpErr == nil {
			content, _ := nested(vpErr, "result", "css", "contentSize").(map[string]any)
			layout, _ := nested(vpErr, "result", "css", "layoutViewport").(map[string]any)
			width, _ := layout["clientWidth"].(float64)
			height, _ := layout["clientHeight"].(float64)
			if cw, ok := content["width"].(float64); ok && width == 0 {
				width = cw
			}
			if ch, ok := content["height"].(float64); ok && height == 0 {
				height = ch
			}
			if width > 0 && height > 0 && (x < 0 || y < 0 || x >= width || y >= height) {
				return nil, fmt.Errorf("x,y (%g,%g) is outside the page viewport (0,0–%gx%g). Coordinates are CSS pixels, not screenshot pixels — screenshots are scaled by the device pixel ratio; divide screenshot coords by (screenshot_width/viewport_width)", x, y, width, height)
			}
		}
		for _, eventType := range []string{"mousePressed", "mouseReleased"} {
			if _, err := s.cdp(ctx, "Input.dispatchMouseEvent", map[string]any{"type": eventType, "x": x, "y": y, "button": "left", "clickCount": 1}, 5000); err != nil {
				return nil, err
			}
		}
		result := map[string]any{"ok": true, "x": x, "y": y}
		if layout, _ := nested(viewport, "result", "css", "layoutViewport").(map[string]any); layout != nil {
			result["viewport"] = map[string]any{"width": layout["clientWidth"], "height": layout["clientHeight"]}
		}
		return result, nil
	case "miniapp_type":
		// text 是必填：schema 与 skills/debugging.md（{text, selector?, clear?}）
		// 都这么声明，缺了它却回 ok:true，agent 会以为文字已经敲进去了。
		if _, present := args["text"]; !present {
			return nil, errors.New(`missing required argument "text"`)
		}
		text := stringArg(args, "text", "")
		selector := stringArg(args, "selector", "")
		// 聚焦与清空都要落到当前页 webview：宿主页里既没有目标元素，也没有
		// 被按键事件聚焦的那个 activeElement。
		var contextID *int
		if selector != "" || boolArg(args, "clear") {
			resolved, err := s.pageContextID(ctx)
			if err != nil {
				return nil, err
			}
			contextID = resolved
		}
		if selector != "" {
			// 组件路径优先：wx-input 一类组件的值不吃键盘事件（实测逐字符
			// keyDown 改不了组件值），走组件的 input 事件才进得去。exparser 桥
			// 不可达（component:false）时退回键盘路径；组件回了空值按失败处理。
			componentExpression := `(function(){var el=document.querySelector(` + jsLiteral(selector) + `);if(!el)return JSON.stringify({error:'Element not found: '+` + jsLiteral(selector) + `});var wxel=el.__wxElement;if(!(wxel&&window.exparser&&typeof window.exparser.triggerEvent==='function'))return JSON.stringify({component:false});window.exparser.triggerEvent(wxel,'input',{value:` + jsLiteral(text) + `});var v=el.value==null?'':String(el.value);return JSON.stringify({component:true,valueLen:v.length})})()`
			value, err := s.evaluateJSON(ctx, componentExpression, false, contextID, 5000)
			if err != nil {
				return nil, err
			}
			entry := objectMap(value)
			if message := stringArg(entry, "error", ""); message != "" {
				return nil, errors.New(message)
			}
			if entry["component"] == true {
				length := 0
				if v, ok := entry["valueLen"].(float64); ok {
					length = int(v)
				}
				if length == 0 {
					return map[string]any{"ok": false, "error": "the input event delivered an empty value", "value_len": 0}, nil
				}
				return map[string]any{"ok": true, "value_len": length, "via": "component-input"}, nil
			}
			// 命中不了元素必须读作失败：静默跳过聚焦会让后面的按键落到别处，
			// 而工具仍然回 ok:true。
			expression := `(function(){var el=document.querySelector(` + jsLiteral(selector) +
				`);if(!el)throw new Error('Element not found: '+` + jsLiteral(selector) + `);el.focus()})()`
			if _, err := s.evaluate(ctx, expression, false, contextID, 5000); err != nil {
				return nil, err
			}
		}
		if boolArg(args, "clear") {
			_, _ = s.evaluate(ctx, `(function(){var el=document.activeElement;if(el&&(el.tagName==='INPUT'||el.tagName==='TEXTAREA')){el.value='';el.dispatchEvent(new Event('input',{bubbles:true}))}})()`, false, contextID, 5000)
		}
		for _, char := range text {
			value := string(char)
			if _, err := s.cdp(ctx, "Input.dispatchKeyEvent", map[string]any{"type": "keyDown", "text": value, "key": value, "unmodifiedText": value}, 5000); err != nil {
				return nil, err
			}
			if _, err := s.cdp(ctx, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": value}, 5000); err != nil {
				return nil, err
			}
		}
		return map[string]any{"ok": true, "typed": text}, nil
	case "miniapp_navigate":
		method := stringArg(args, "method", "navigateTo")
		route := stringArg(args, "route", "")
		delta := intArg(args, "delta", 1)
		if delta < 1 {
			delta = 1
		}
		if method != "navigateBack" && route == "" {
			return nil, fmt.Errorf("missing required argument \"route\" for %s", method)
		}
		// 回调写 ok 不等于栈真的变了：实测 navigateTo 回 ok 时对应层路由还是
		// 空串、switchTab/navigateBack 回 ok 而栈纹丝不动。以调用之后的真实
		// 路由为准——先记下分发前的栈深，回退按深度收敛判定。
		beforeDepth, beforeTop := s.stackShape(ctx)
		// 一律经页面侧导航钩子，由它解析 wxFrame 并区分 tabBar 页。以前 switchTab
		// 单独向 CDP 求值 `wx.switchTab`：既落在没有 wx 的宿主页上下文（必然回
		// "wx is not defined"），又绕开了钩子，于是守卫开启时连工具自己的跳转都会
		// 被自己的守卫拦下、并且假报成功。
		_, err := s.appCall(ctx, "navigator.navigate", map[string]any{"route": route, "method": method, "delta": delta})
		result := map[string]any{"route": route, "method": method}
		if err != nil {
			// 插件页在前台时，宿主侧的返回/切换会被拒（"no permission currently"）
			// ——改在该页 webview 上下文里调 navigateBack，实测这样才回得动。
			if method == "navigateBack" {
				if rbErr := s.navigateBackInPageWebview(ctx, delta); rbErr == nil {
					result["ok"] = true
					result["note"] = "dispatched inside the page webview (the appservice dispatch was refused)"
					return result, nil
				}
			}
			return nil, err
		}
		observed, verifyErr := s.verifyNavigation(ctx, method, strings.TrimPrefix(route, "/"), delta, beforeDepth, beforeTop)
		if verifyErr != nil {
			result["ok"] = false
			result["error"] = verifyErr.Error()
			result["route_observed"] = observed
			return result, nil
		}
		result["ok"] = true
		result["route_observed"] = observed
		return result, nil
	case "miniapp_get_routes":
		result, err := s.appCall(ctx, "navigator.pages", map[string]any{})
		if err != nil {
			return nil, err
		}
		data := objectMap(result)
		pages := anySlice(data["pages"])
		return map[string]any{"pages": pages, "tab_bar_pages": data["tab_bar_pages"], "total": len(pages)}, nil
	case "miniapp_get_current_route":
		return s.appCall(ctx, "navigator.currentRoute", map[string]any{})
	case "miniapp_scroll":
		x := numberArgDefault(args, "x", 0)
		y := numberArgDefault(args, "y", 300)
		selector := stringArg(args, "selector", "")
		contextID, err := s.pageContextID(ctx)
		if err != nil {
			return nil, err
		}
		// 页面由合成器滚动：改 scrollTop / 调 window.scrollBy 都不会让页面动
		// （真机实测两者都恒为 0），所以派发真实滚轮事件——与坐标点击同一机制。
		// 落点只取页面内的一个位置：滚轮滚的是落点下方的滚动容器，因此不需要
		// 精确的坐标映射，只要点落在页面区域内即可。
		locate := `var p={x:Math.round(window.innerWidth/2),y:Math.round(window.innerHeight/2)};`
		if selector != "" {
			locate = `var el=document.querySelector(` + jsLiteral(selector) + `);` +
				`if(!el)return JSON.stringify({error:'Element not found'});` +
				`var r=el.getBoundingClientRect();` +
				`var p={x:Math.round(r.left+r.width/2),y:Math.round(r.top+r.height/2)};`
		}
		point, err := s.evaluateJSON(ctx, `(function(){`+locate+`return JSON.stringify(p)})()`, false, contextID, 5000)
		if err != nil {
			return nil, err
		}
		if message := stringArg(objectMap(point), "error", ""); message != "" {
			return nil, errors.New(message)
		}
		pointMap := objectMap(point)
		if _, err := s.cdp(ctx, "Input.dispatchMouseEvent", map[string]any{
			"type":   "mouseWheel",
			"x":      numberArgDefault(pointMap, "x", 0),
			"y":      numberArgDefault(pointMap, "y", 0),
			"deltaX": x,
			"deltaY": y,
		}, 5000); err != nil {
			return nil, err
		}
		// 不回读滚动位置：页面由合成器滚动，页内的 window.scrollY / scrollTop 恒为 0
		// （真机实测，滚动确实发生了也一样），回读只会把「滚了」报成「没滚」。
		// 落定与否由调用方用 miniapp_screenshot 核对。
		return map[string]any{
			"ok":     true,
			"wheelX": pointMap["x"],
			"wheelY": pointMap["y"],
			"deltaX": x,
			"deltaY": y,
		}, nil
	case "miniapp_list_contexts":
		if s.deps.Core == nil {
			return nil, fmt.Errorf("core engine unavailable")
		}
		_, _ = s.cdp(ctx, "Runtime.enable", map[string]any{}, 5000)
		// 事件路径：Core 桥吃 Runtime.executionContext* 事件维护的注册表——
		// 权威、零探测。列表项补 id/种类/路由（webview 用 body[is]）。
		if items, err := s.deps.Core.Contexts(ctx); err == nil && len(items) > 0 {
			entries := make([]map[string]any, 0, len(items))
			for _, item := range items {
				id, ok := contextIDValue(item["id"])
				if !ok {
					continue
				}
				entry := map[string]any{"id": id}
				if s.contextAnswers(ctx, id, appServiceProbe) {
					entry["kind"] = "appservice"
				} else {
					entry["kind"] = "webview"
					entry["route"] = s.contextRoute(ctx, id)
				}
				entries = append(entries, entry)
			}
			if len(entries) > 0 {
				sort.Slice(entries, func(i, j int) bool { return entries[i]["id"].(int) < entries[j]["id"].(int) })
				return map[string]any{"contexts": entries, "count": len(entries), "source": "events"}, nil
			}
		}
		// 回退路径：WMPF 的 CDP shim 不保证发 context 事件——有界探测（1..24，
		// 每探针 3s），行为与事件路径落地前一致。
		contexts := []map[string]any{}
		for id := 1; id <= contextProbeMaxID; id++ {
			value, err := s.evaluate(ctx, `(function(){return JSON.stringify({hasWx:typeof wx!=='undefined',hasGetApp:typeof getApp!=='undefined',loc:typeof location!=='undefined'?location.href:'N/A'})})()`, false, &id, 3000)
			if err != nil {
				continue
			}
			text, ok := value.(string)
			if !ok || text == "" {
				continue
			}
			var info map[string]any
			if json.Unmarshal([]byte(text), &info) != nil {
				continue
			}
			kind := "webview"
			if info["hasGetApp"] == true {
				kind = "appservice"
			}
			contexts = append(contexts, map[string]any{"id": id, "name": kind, "origin": info["loc"], "type": kind})
		}
		return map[string]any{"contexts": contexts, "count": len(contexts), "source": "probe"}, nil
	case "miniapp_evaluate":
		var contextID *int
		if value, ok := numberArg(args, "context_id"); ok {
			id := int(value)
			contextID = &id
		}
		value, valueType, exception, err := s.evaluateDetail(ctx, stringArg(args, "expression", ""), boolArg(args, "await_promise"), contextID, 15000)
		if err != nil {
			return nil, err
		}
		if exception != "" {
			return map[string]any{"error": "Evaluation error", "exception": exception}, nil
		}
		if capped, over := oversizedResult(value, evaluateResultCap); over {
			return capped, nil
		}
		return map[string]any{"value": value, "type": valueType}, nil
	case "miniapp_call":
		// 业务调用走 appservice：小程序自己的方法 / store.dispatch 才会过它自己的
		// 请求拦截器（裸调 wx.request 不带令牌，实测如此）。回包只回结果的形状
		// ——类型、长度、键名——令牌不出小程序。
		contextID, err := s.appServiceContextID(ctx)
		if err != nil {
			return nil, err
		}
		value, _, exception, err := s.evaluateDetail(ctx, stringArg(args, "expression", ""), boolArg(args, "await_promise"), contextID, 15000)
		if err != nil {
			return nil, err
		}
		if exception != "" {
			return map[string]any{"error": "Evaluation error", "exception": exception}, nil
		}
		// 页侧助手习惯把结果 JSON.stringify 后回字符串：能解析就按解析后的形状摘要。
		if text, ok := value.(string); ok {
			var parsed any
			if json.Unmarshal([]byte(text), &parsed) == nil {
				value = parsed
			}
		}
		return map[string]any{"ok": true, "summary": summarizeCallValue(value, 0)}, nil
	case "miniapp_console_log":
		if boolArg(args, "clear") {
			if _, err := s.appCall(ctx, "console.clear", map[string]any{}); err != nil {
				return nil, err
			}
		}
		// The shell drains the page-side console hook into its own ring, so the
		// tool reads what the GUI console panel reads. (The previous
		// implementation read window.__mcp_console_buffer, which nothing ever
		// wrote, and always answered with an empty list.)
		// tail：要的是「最近 100 条」。从 0 开始的增量读在环形缓冲写满后
		// 拿到的是最旧一批，与工具描述相反。
		result, err := s.appCall(ctx, "console.list", map[string]any{"tail": 100})
		if err != nil {
			return nil, err
		}
		records := anySlice(objectMap(result)["records"])
		redacted := redactEnabled(args)
		logs := make([]any, 0, len(records))
		for _, item := range records {
			entry := objectMap(item)
			if record, ok := entry["record"].(map[string]any); ok {
				if redacted {
					if masked, ok := redactAny(record).(map[string]any); ok {
						record = masked
					}
				}
				logs = append(logs, record)
				continue
			}
			if redacted {
				item = redactAny(item)
			}
			logs = append(logs, item)
		}
		coverage := consoleCoverage(objectMap(result))
		if truncated, exists := objectMap(result)["truncated"]; exists {
			coverage["truncated"] = truncated
		}
		return map[string]any{"logs": logs, "count": len(logs), "redacted": redacted, "coverage": coverage}, nil
	case "miniapp_get_info":
		result, err := s.appCall(ctx, "navigator.pages", map[string]any{})
		if err != nil {
			return nil, err
		}
		data := objectMap(result)
		data["pages_count"] = len(anySlice(data["pages"]))
		return data, nil
	case "miniapp_page_stack":
		return s.pageSnapshot(ctx, redactEnabled(args))
	case "miniapp_a11y_tree":
		return s.a11yTree(ctx)
	case "miniapp_get_storage":
		redacted := redactEnabled(args)
		// key 缺省 = 全量（合并旧 miniapp_get_storage_key 的单键形态）；
		// mode:"keys" 只回键名与体长——先看有什么再决定要不要看内容。
		if stringArg(args, "mode", "full") == "keys" {
			result, err := s.evaluateStorage(ctx, `(function(){try{var i=wx.getStorageInfoSync(),k=i.keys||[],s={};for(var n=0;n<k.length;n++){try{s[k[n]]=JSON.stringify(wx.getStorageSync(k[n])).length}catch(e){s[k[n]]=-1}}return JSON.stringify({keys:k,sizes:s,currentSize:i.currentSize||0})}catch(e){return JSON.stringify({error:e.message})}})()`, 8000)
			if err != nil {
				return nil, err
			}
			if data := objectMap(result); data != nil {
				data["mode"] = "keys"
				return data, nil
			}
			return result, nil
		}
		if key := stringArg(args, "key", ""); key != "" {
			result, err := s.evaluateStorage(ctx, `(function(){try{var v=wx.getStorageSync(`+jsLiteral(key)+`);return JSON.stringify({key:`+jsLiteral(key)+`,value:v,type:typeof v})}catch(e){return JSON.stringify({error:e.message})}})()`, 5000)
			if err != nil {
				return nil, err
			}
			if redacted {
				result = redactAny(result)
				// 单键读的 storage 键名本身敏感（读的就是 token）时值无条件掩：
				// 回包字段名是通用的 "value"，键名规则看不见它。
				if sensitiveKey(key) {
					if m := objectMap(result); m != nil {
						m["value"] = redactSensitiveValue(m["value"])
					}
				}
			}
			if capped, over := oversizedResult(result, storageResultCap); over {
				return capped, nil
			}
			return stampRedacted(result, redacted), nil
		}
		result, err := s.evaluateStorage(ctx, `(function(){try{var i=wx.getStorageInfoSync(),r={},k=i.keys||[];for(var n=0;n<k.length;n++){try{r[k[n]]=wx.getStorageSync(k[n])}catch(e){}}return JSON.stringify({keys:k,data:r,currentSize:i.currentSize||0})}catch(e){return JSON.stringify({error:e.message})}})()`, 8000)
		if err != nil {
			return nil, err
		}
		if redacted {
			result = redactAny(result)
		}
		if capped, over := oversizedResult(result, storageResultCap); over {
			return capped, nil
		}
		return stampRedacted(result, redacted), nil
	case "miniapp_get_storage_key":
		key := stringArg(args, "key", "")
		result, err := s.evaluateStorage(ctx, `(function(){try{var v=wx.getStorageSync(`+jsLiteral(key)+`);return JSON.stringify({key:`+jsLiteral(key)+`,value:v,type:typeof v})}catch(e){return JSON.stringify({error:e.message})}})()`, 5000)
		if err != nil {
			return nil, err
		}
		// 合并形态 miniapp_get_storage 的单键读有同一顶 32KB 帽，旧名不得成为
		// 绕开它的口子；打码同口径——旧名同样不是绕开打码的口子。
		redacted := redactEnabled(args)
		if redacted {
			result = redactAny(result)
			if sensitiveKey(key) {
				if m := objectMap(result); m != nil {
					m["value"] = redactSensitiveValue(m["value"])
				}
			}
		}
		if capped, over := oversizedResult(result, storageResultCap); over {
			return capped, nil
		}
		return stampRedacted(result, redacted), nil
	case "miniapp_set_storage":
		// 授权测试的篡改入口：改 token / 角色位 / 开关后复现行为。value 以
		// JSON 文本进页面再 parse，保住对象/数组/数字的形状。
		key := stringArg(args, "key", "")
		if key == "" {
			return nil, fmt.Errorf("missing required argument \"key\"")
		}
		valueJSON, err := json.Marshal(args["value"])
		if err != nil {
			return nil, fmt.Errorf("value is not JSON-serializable: %w", err)
		}
		return s.evaluateStorage(ctx, `(function(){try{wx.setStorageSync(`+jsLiteral(key)+`, JSON.parse(`+jsLiteral(string(valueJSON))+`));return JSON.stringify({ok:true,key:`+jsLiteral(key)+`})}catch(e){return JSON.stringify({error:e.message||String(e)})}})()`, 5000)
	case "miniapp_remove_storage":
		// all:true = 全量清空（合并旧 miniapp_clear_storage 的破坏性形态）。
		if args["all"] == true {
			return s.evaluateStorage(ctx, `(function(){try{wx.clearStorageSync();return JSON.stringify({ok:true,cleared:"all"})}catch(e){return JSON.stringify({error:e.message||String(e)})}})()`, 5000)
		}
		key := stringArg(args, "key", "")
		if key == "" {
			return nil, fmt.Errorf("provide key, or all:true to wipe everything")
		}
		return s.evaluateStorage(ctx, `(function(){try{wx.removeStorageSync(`+jsLiteral(key)+`);return JSON.stringify({ok:true,key:`+jsLiteral(key)+`})}catch(e){return JSON.stringify({error:e.message||String(e)})}})()`, 5000)
	case "miniapp_clear_storage":
		return s.evaluateStorage(ctx, `(function(){try{wx.clearStorageSync();return JSON.stringify({ok:true})}catch(e){return JSON.stringify({error:e.message||String(e)})}})()`, 5000)
	case "miniapp_call_cloud":
		result, err := s.appCall(ctx, "cloud.call", map[string]any{"name": stringArg(args, "name", ""), "data": args["data"]})
		if err != nil {
			return nil, err
		}
		// 云函数的返回值与 evaluate 的返回值同一个性质：页面侧没有天然上限。
		if capped, over := oversizedResult(result, evaluateResultCap); over {
			return capped, nil
		}
		return result, nil
	case "miniapp_cloud_captures":
		if s.deps.Core == nil {
			return nil, fmt.Errorf("core engine unavailable")
		}
		// 快照式读没有游标：页宽封顶并如实上报，要全量改走 hook_drain 的游标
		// 翻页——一次调用灌回上千条全量 body 正是整形纪律要拦的形态。
		page, err := s.deps.Core.HookDrain(ctx, "cloud", 0, cloudCapturesCap, 0, 0)
		if err != nil {
			return nil, err
		}
		captures := make([]map[string]any, 0, len(page.Records))
		for _, record := range page.Records {
			captures = append(captures, record.Record)
		}
		// 边界自守：帽在切这里生效，不依赖 Core 对 limit 的自律。
		truncated := len(captures) > cloudCapturesCap
		if truncated {
			captures = captures[:cloudCapturesCap]
		}
		redacted := redactEnabled(args)
		if redacted {
			for index := range captures {
				if masked, ok := redactAny(captures[index]).(map[string]any); ok {
					captures[index] = masked
				}
			}
		}
		if boolArg(args, "clear") {
			_ = s.deps.Core.HookClear(ctx, "cloud")
		}
		result := map[string]any{"captures": captures, "count": len(captures), "truncated": truncated, "redacted": redacted}
		if truncated {
			result["note"] = "captures reached the page cap; page hook_drain {name:\"cloud\"} by cursor for the rest"
		}
		return result, nil
	case "miniapp_cloud_scan":
		if s.deps.Core == nil {
			return nil, fmt.Errorf("core engine unavailable")
		}
		items, err := s.deps.Core.CloudScan(ctx)
		return map[string]any{"items": items, "count": len(items)}, err
	case "miniapp_http_request":
		return httpTool(ctx, args)
	case "miniapp_decompile":
		appid := stringArg(args, "appid", "")
		packagesDir := stringArg(args, "packages_dir", "")
		if packagesDir == "" {
			if value, err := s.appCall(ctx, "extract.defaultDir", map[string]any{}); err == nil {
				packagesDir, _ = objectMap(value)["dir"].(string)
			}
		}
		if packagesDir == "" {
			//nolint:staticcheck // Message text is part of the contract; clients match on it.
			return nil, errors.New("Cannot find wxapkg packages directory. Provide packages_dir parameter.")
		}
		listed, err := s.appCall(ctx, "extract.packages", map[string]any{"dir": packagesDir})
		if err != nil {
			return nil, err
		}
		packages := anySlice(objectMap(listed)["packages"])
		matched := []map[string]any{}
		availableSet := map[string]bool{}
		for _, item := range packages {
			pkg := objectMap(item)
			candidate, _ := pkg["appid"].(string)
			if candidate != "" {
				availableSet[candidate] = true
			}
			if candidate == appid {
				matched = append(matched, pkg)
			}
		}
		if len(matched) == 0 {
			available := make([]string, 0, len(availableSet))
			for candidate := range availableSet {
				available = append(available, candidate)
			}
			sort.Strings(available)
			if len(available) > 20 {
				available = available[:20]
			}
			return map[string]any{"error": "No wxapkg found for " + appid, "available_appids": available}, nil
		}
		// Refused here rather than deep in the pipeline: the templates cannot be
		// restored, so the decompile would fail and leave no output at all.
		if flag, _ := matched[0]["unsupported"].(bool); flag {
			//nolint:staticcheck // Message text is part of the contract; clients match on it.
			return nil, errors.New("This mini program's page templates were built by WeChat's newer template runtime, which WxTap cannot restore, so decompiling it would fail. extract_inventory marks it unsupported.")
		}
		result, err := s.appCall(ctx, "extract.decompile", map[string]any{"appid": appid, "dir": packagesDir})
		if err != nil {
			return nil, err
		}
		data := objectMap(result)
		outputDir, _ := data["output_dir"].(string)
		if outputDir == "" {
			outputDir, _ = data["dir"].(string)
		}
		filesCount := 0
		switch count := data["files_count"].(type) {
		case float64:
			filesCount = int(count)
		case int:
			filesCount = count
		}
		packagesProcessed := len(matched)
		switch count := data["packages_processed"].(type) {
		case float64:
			packagesProcessed = int(count)
		case int:
			packagesProcessed = count
		}
		// 页面→文件索引随反编译生成（page-index.json，主包/子包/插件分组）；
		// 摘要进回包，全量清单用 miniapp_read_file 读。
		pageIndex := map[string]any{"file": "page-index.json"}
		if outputDir != "" {
			if raw, err := os.ReadFile(filepath.Join(outputDir, "page-index.json")); err == nil {
				var index struct {
					Pages    []map[string]any `json:"pages"`
					Packages map[string]int   `json:"packages"`
				}
				if json.Unmarshal(raw, &index) == nil {
					pageIndex["pages"] = len(index.Pages)
					pageIndex["packages"] = index.Packages
				}
			}
		}
		return map[string]any{"output_dir": outputDir, "files_count": filesCount, "packages_processed": packagesProcessed, "page_index": pageIndex}, nil
	case "miniapp_scan_sensitive":
		if s.deps.Scan == nil {
			return nil, fmt.Errorf("scanner unavailable")
		}
		paths, err := s.appCall(ctx, "settings.getPaths", map[string]any{})
		if err != nil {
			return nil, err
		}
		pathMap := objectMap(paths)
		root, _ := pathMap["outputDir"].(string)
		if root == "" {
			return nil, fmt.Errorf("output directory unavailable")
		}
		appid := stringArg(args, "appid", "")
		if err := ipc.ValidateAppID(appid); err != nil {
			return nil, err
		}
		scanPath := filepath.Join(root, appid)
		// Scanning an appid that has not been decompiled yet is refused;
		// a clean 0-file report would hide the missing step.
		if info, err := os.Stat(scanPath); err != nil || !info.IsDir() {
			return nil, errors.New("Decompiled output not found. Run miniapp_decompile first for " + appid)
		}
		return s.requestScan(scanPath), nil
	case "miniapp_read_file":
		path := stringArg(args, "path", "")
		// The tool's contract is "read a decompiled source file": without a
		// root check the path is an unrestricted file read, and audited
		// (potentially hostile) package code could steer the agent into
		// exfiltrating local files.
		if err := s.ensureWithinCodeRoot(path); err != nil {
			return nil, err
		}
		// A missing path answers with a plain error; an empty success
		// would read as an empty file to an MCP client.
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("File not found: " + path)
		}
		result, err := s.appCall(ctx, "code.readFile", map[string]any{"path": path})
		if err != nil {
			return nil, err
		}
		data := objectMap(result)
		content, _ := data["content"].(string)
		// 图片在 code.readFile 里带内联 data URL —— 那是给界面画图用的。几 MB 的
		// base64 进 MCP 结果只会占满上下文，而这个工具的契约是读源码。kind 必须
		// 留下：content 为空会被客户端读成空文件（见上面的 missing 分支）。
		if kind, _ := data["kind"].(string); kind == "image" {
			delete(data, "dataUrl")
			if content == "" {
				content = "（图片文件，未作为源码返回）"
			}
		}
		maxLength := intArg(args, "max_length", 100000)
		truncated := maxLength > 0 && len(content) > maxLength
		if truncated {
			content = content[:maxLength]
			// 按字节切会把多字节字符劈成半个，JSON 里就成了替换字符；退到字符边界。
			// UTF-8 最长 4 字节，最多退 3 次；空串本身合法，循环不会越界。
			for i := 0; i < utf8.UTFMax && !utf8.ValidString(content); i++ {
				content = content[:len(content)-1]
			}
		}
		data["content"] = content
		data["path"] = path
		data["truncated"] = truncated
		return data, nil
	case "miniapp_search_code":
		root := stringArg(args, "root", "")
		// Same root discipline as miniapp_read_file: the search root is a
		// decompiled output tree, not an arbitrary directory.
		if err := s.ensureWithinCodeRoot(root); err != nil {
			return nil, err
		}
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			return nil, errors.New("Directory not found: " + root)
		}
		result, err := s.appCall(ctx, "code.search", map[string]any{"root": root, "query": stringArg(args, "query", ""), "regex": boolArg(args, "regex")})
		if err != nil {
			return nil, err
		}
		data := objectMap(result)
		if message, ok := data["error"].(string); ok && message != "" {
			return nil, errors.New(message)
		}
		results := anySlice(data["results"])
		maxResults := intArg(args, "max_results", 200)
		// 后端 search 自带 500 条上限，任一环节截断都要如实上报。
		truncated, _ := data["truncated"].(bool)
		if maxResults > 0 && len(results) > maxResults {
			results = results[:maxResults]
			truncated = true
		}
		return map[string]any{"results": results, "total": len(results), "truncated": truncated}, nil
	case "miniapp_list_packages":
		packagesDir := stringArg(args, "packages_dir", "")
		if packagesDir == "" {
			if value, err := s.appCall(ctx, "extract.defaultDir", map[string]any{}); err == nil {
				packagesDir, _ = objectMap(value)["dir"].(string)
			}
		}
		if packagesDir == "" {
			//nolint:staticcheck // Message text is part of the contract; clients match on it.
			return nil, errors.New("Cannot find packages directory. Provide packages_dir or ensure WeChat is installed.")
		}
		result, err := s.appCall(ctx, "extract.packages", map[string]any{"dir": packagesDir})
		if err != nil {
			return nil, err
		}
		packages := anySlice(objectMap(result)["packages"])
		details := map[string][]map[string]any{}
		for _, item := range packages {
			pkg := objectMap(item)
			appid, _ := pkg["appid"].(string)
			if appid == "" {
				appid = "unknown"
			}
			details[appid] = append(details[appid], map[string]any{"name": pkg["name"], "size": pkg["size"], "path": pkg["path"]})
		}
		appids := make([]string, 0, len(details))
		for appid := range details {
			appids = append(appids, appid)
		}
		sort.Strings(appids)
		return map[string]any{"packages_dir": packagesDir, "appids": appids, "total_packages": len(packages), "details": details}, nil
	case "miniapp_get_skills":
		return readSkills(s.deps.SkillsDir)
	case "miniapp_set_breakpoint":
		// 走 debugger_breakpoint 的同一实现：注册表（{action:"list"}）必须
		// 看得见每个由本服务设置的断点，旧名不得成为旁路。line 以 float64
		// 透传——args 里的数值一律是 JSON 形状。
		return s.debuggerBreakpoint(ctx, map[string]any{
			"action":    "set",
			"url":       stringArg(args, "url", ""),
			"line":      numberArgDefault(args, "line", 1),
			"condition": stringArg(args, "condition", ""),
		})
	case "miniapp_remove_breakpoint":
		return s.debuggerBreakpoint(ctx, map[string]any{
			"action":        "remove",
			"breakpoint_id": stringArg(args, "breakpoint_id", ""),
		})
	case "miniapp_get_source":
		pattern := stringArg(args, "url_pattern", "")
		if strings.TrimSpace(pattern) == "" {
			return nil, errors.New(`missing required argument "url_pattern"`)
		}
		// scriptId 只能来自 Debugger.scriptParsed 流（Core 在 CDP 层捕获）；
		// 直接传 scriptId 也接受。
		scriptID, matchedURL, listed := s.matchScript(ctx, pattern)
		if scriptID == "" && isScriptID(pattern) {
			scriptID = pattern
		}
		if scriptID == "" {
			// 找不到目标必须报错。以前这里把 pattern 当 scriptId 去取正文，于是
			// 「没匹配到」变成「成功但正文为空」，读起来像「这个脚本是空的」。
			if listed == 0 {
				return nil, errors.New("脚本清单还是空的：Debugger 域刚启用，scriptParsed 事件尚未到达。稍后重试，或先用 debugger_list_scripts 确认清单")
			}
			return nil, fmt.Errorf("没有脚本的 url 匹配 %q（清单里 %d 个）；用 debugger_list_scripts 查看可用 url", pattern, listed)
		}
		resp, err := s.cdp(ctx, "Debugger.getScriptSource", map[string]any{"scriptId": scriptID}, 10000)
		if err != nil {
			return nil, err
		}
		source, _ := nested(resp, "result", "scriptSource").(string)
		if source == "" {
			// 命中却取不到正文同样是失败：空正文回成功等于谎报。
			return nil, fmt.Errorf("script %s 取到空正文", scriptID)
		}
		maxLength := intArg(args, "max_length", 50000)
		truncated := maxLength > 0 && len(source) > maxLength
		if truncated {
			source = source[:maxLength]
			// 与 miniapp_read_file 同一纪律：字节切会把多字节字符劈成半个。
			for i := 0; i < utf8.UTFMax && !utf8.ValidString(source); i++ {
				source = source[:len(source)-1]
			}
		}
		return map[string]any{"source": source, "scriptId": scriptID, "url": matchedURL, "truncated": truncated}, nil
	default:
		return nil, nil
	}
}

// pageSnapshot 合并两个页面读：navigator.pageStack（运行时栈 {stack,current}）
// 与 navigator.pages（配置页 {pages,tab_bar_pages,current_route}）。两边各自
// best-effort：一边失败不拖垮另一边，失败以 *_error 字段内联。
// pageSnapshot 是 miniapp_page_stack 的实现：redacted 时栈上各层的 query
// 值打码（键名保留）——证照页跳统一认证把会话类参数挂在 query 上的场景，
// 实值不该原样落进 agent transcript。
func (s *Server) pageSnapshot(ctx context.Context, redacted bool) (any, error) {
	stackRes, stackErr := s.appCall(ctx, "navigator.pageStack", map[string]any{})
	pagesRes, pagesErr := s.appCall(ctx, "navigator.pages", map[string]any{})
	if stackErr != nil && pagesErr != nil {
		return nil, stackErr
	}
	out := map[string]any{}
	if stackErr != nil {
		out["stack_error"] = stackErr.Error()
	} else {
		data := objectMap(stackRes)
		stack := data["stack"]
		if redacted {
			stack = redactAny(stack)
		}
		out["stack"] = stack
		out["stack_depth"] = len(anySlice(data["stack"]))
		out["current_route"] = data["current"]
	}
	if pagesErr != nil {
		out["configured_error"] = pagesErr.Error()
	} else {
		data := objectMap(pagesRes)
		out["configured"] = data["pages"]
		out["tab_bar_pages"] = data["tab_bar_pages"]
		if value, has := out["current_route"]; !has || value == "" || value == nil {
			out["current_route"] = data["current_route"]
		}
	}
	out["redacted"] = redacted
	return out, nil
}

// CDP hands out one execution context per realm, and a mini program spreads its
// work across three kinds of them:
//
//	host page     the WMPF shell that hosts the page webviews — no wx at all
//	appservice    the logic layer — the real wx, plus getApp/getCurrentPages
//	page webview  one per open page — the rendered DOM, body[is] = its route
//
// Runtime.evaluate without a contextId lands in the host page. That is why
// reaching for wx there answered "wx is not defined" and why querySelector
// never matched a mini program element. The two resolvers below find the
// context a call actually needs; both cache, because a cold scan costs one
// round-trip per candidate id.
const (
	// contextProbeMaxID bounds the cold scan. Ids are handed out in creation
	// order and WMPF keeps the appservice plus a handful of page webviews
	// alive, so a small ceiling is enough; the scan is the fallback, not the
	// steady state.
	contextProbeMaxID = 24
	// contextProbeTimeoutMs stays short: an id that does not exist fails
	// immediately, and one wedged realm must not stall the whole tool.
	contextProbeTimeoutMs = 3000
	// contextProbeYes is the answer both probes below look for. Matching an
	// exact token (rather than truthiness) keeps a realm that answers with an
	// unrelated value from being mistaken for a match.
	contextProbeYes = "yes"
	// pageContextAttempts / pageContextRetryDelay bound the retry around a page
	// transition. Switching pages leaves a short window where the appservice
	// still reports the outgoing route while that page's webview is already
	// gone: an exact match cannot succeed there, and waiting one beat is what
	// makes it succeed. Waiting is also the only safe answer — falling back to
	// "whichever page webview is alive" would drive a page the mini program is
	// not showing.
	pageContextAttempts   = 3
	pageContextRetryDelay = 250 * time.Millisecond
	// scriptMatchAttempts / scriptMatchDelay bound the reread after the Debugger
	// domain is enabled: its scriptParsed events arrive asynchronously, so the
	// first read after a fresh attach routinely finds an empty ring.
	scriptMatchAttempts = 3
	scriptMatchDelay    = 250 * time.Millisecond
)

// findContext evaluates probe in every execution context and returns the
// highest id that answers "yes", or 0. Highest rather than first: contexts are
// created in order, so when two live webviews show the same route (a page
// pushed twice) the later one is what the mini program is displaying.
func (s *Server) findContext(ctx context.Context, probe string) int {
	_, _ = s.cdp(ctx, "Runtime.enable", map[string]any{}, 5000)
	found := 0
	for id := 1; id <= contextProbeMaxID; id++ {
		if s.contextAnswers(ctx, id, probe) {
			found = id
		}
	}
	return found
}

// contextAnswers runs probe in one specific context, so a cached id is
// re-validated with a single round-trip instead of a rescan.
func (s *Server) contextAnswers(ctx context.Context, id int, probe string) bool {
	candidate := id
	value, err := s.evaluate(ctx, probe, false, &candidate, contextProbeTimeoutMs)
	if err != nil {
		return false
	}
	text, ok := value.(string)
	return ok && text == contextProbeYes
}

// contextIDValue coerces a Core contexts entry id (JSON-decoded numbers are
// float64; the RPC may also hand back int64).
func contextIDValue(raw any) (int, bool) {
	switch value := raw.(type) {
	case float64:
		return int(value), true
	case int64:
		return int(value), true
	case int:
		return value, true
	}
	return 0, false
}

// contextRoute reads one webview context's body[is] marker — the route that
// page is showing. Empty when the context has no DOM (the appservice) or the
// marker is absent.
func (s *Server) contextRoute(ctx context.Context, id int) string {
	candidate := id
	value, err := s.evaluateJSON(ctx, `(function(){return (document.body&&document.body.getAttribute("is"))||""})()`, false, &candidate, contextProbeTimeoutMs)
	if err != nil {
		return ""
	}
	route, _ := value.(string)
	return route
}

const appServiceProbe = `(function(){return typeof wx==='object'&&typeof getApp==='function'?` +
	`'` + contextProbeYes + `':'no'})()`

// appServiceContextID resolves the mini program's logic layer, where the real
// wx lives. Storage and every other wx.* call belong there: the host page has
// no wx, and a page webview's wx is the render-layer object, not the one that
// owns the mini program's state.
func (s *Server) appServiceContextID(ctx context.Context) (*int, error) {
	s.contextMu.Lock()
	cached := s.appServiceCtx
	s.contextMu.Unlock()
	if cached != 0 && s.contextAnswers(ctx, cached, appServiceProbe) {
		return &cached, nil
	}
	id := s.findContext(ctx, appServiceProbe)
	if id == 0 {
		return nil, errors.New("mini program logic layer not found: no execution context exposes wx + getApp (is a mini program open?)")
	}
	s.contextMu.Lock()
	s.appServiceCtx = id
	s.contextMu.Unlock()
	return &id, nil
}

// pageContextID resolves the webview showing the mini program's current page.
// The page's DOM exists only there — the host page has no wx-view elements at
// all, which is why the selector-driven tools could never match anything.
//
// The route comes from the appservice (a page webview carries no marker for it)
// and is matched against the body[is] attribute WMPF stamps on every page
// frame. Matching on that attribute is what makes this the current page rather
// than any other live webview: ids are not stable across navigations, and
// several pages stay alive at once.
func (s *Server) pageContextID(ctx context.Context) (*int, error) {
	lastRoute := ""
	for attempt := 0; attempt < pageContextAttempts; attempt++ {
		route, err := s.currentRoute(ctx)
		if err != nil {
			return nil, err
		}
		if route == "" {
			return nil, errors.New("current page route is unknown: cannot tell which page webview to drive")
		}
		lastRoute = route
		probe := `(function(){return document.body&&document.body.getAttribute('is')===` +
			jsLiteral(route) + `?'` + contextProbeYes + `':'no'})()`
		s.contextMu.Lock()
		cached, cachedRoute := s.pageCtx, s.pageCtxRoute
		s.contextMu.Unlock()
		if cached != 0 && cachedRoute == route && s.contextAnswers(ctx, cached, probe) {
			return &cached, nil
		}
		if id := s.findContext(ctx, probe); id != 0 {
			s.contextMu.Lock()
			s.pageCtx, s.pageCtxRoute = id, route
			s.contextMu.Unlock()
			return &id, nil
		}
		if attempt+1 < pageContextAttempts {
			time.Sleep(pageContextRetryDelay)
		}
	}
	return nil, fmt.Errorf("page webview for %s not found after %d attempts: cannot tell which page webview to drive (a page transition may still be settling)", lastRoute, pageContextAttempts)
}

// currentRoute reads the route the mini program is actually on. window.nav owns
// this reading: the configured page list is not the runtime stack, and guessing
// from it would target the wrong webview after a navigateBack.
func (s *Server) currentRoute(ctx context.Context) (string, error) {
	result, err := s.appCall(ctx, "navigator.currentRoute", map[string]any{})
	if err != nil {
		return "", err
	}
	route, _ := objectMap(result)["route"].(string)
	// body[is] carries the route without a leading slash; the IPC reading may
	// include one.
	return strings.TrimPrefix(strings.TrimSpace(route), "/"), nil
}

// matchScript resolves a url pattern against the Core's script ring, returning
// the script id, the matched url and how many scripts were listed. It enables
// the Debugger domain first — that is what makes the target report its scripts
// at all — and rereads while the ring is still empty.
func (s *Server) matchScript(ctx context.Context, pattern string) (string, string, int) {
	if s.deps.Core == nil {
		return "", "", 0
	}
	lowered := strings.ToLower(pattern)
	listed := 0
	for attempt := 0; attempt < scriptMatchAttempts; attempt++ {
		state, err := s.deps.Core.DebugState(ctx, true)
		if err != nil {
			return "", "", listed
		}
		scripts := anySlice(state["scripts"])
		listed = len(scripts)
		for _, script := range scripts {
			entry := objectMap(script)
			url := stringArg(entry, "url", "")
			if url == "" {
				continue
			}
			if url == pattern || strings.Contains(strings.ToLower(url), lowered) {
				return stringArg(entry, "scriptId", ""), url, listed
			}
		}
		if listed > 0 {
			break
		}
		if attempt+1 < scriptMatchAttempts {
			time.Sleep(scriptMatchDelay)
		}
	}
	return "", "", listed
}

// isScriptID reports whether the pattern is a bare script id, which
// miniapp_get_source accepts alongside a url pattern.
func isScriptID(pattern string) bool {
	if pattern == "" {
		return false
	}
	for _, r := range pattern {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (s *Server) cdp(ctx context.Context, method string, params map[string]any, timeoutMs int) (map[string]any, error) {
	if s.deps.Core == nil {
		return nil, fmt.Errorf("core engine unavailable")
	}
	return s.deps.Core.CDPCommand(ctx, method, params, timeoutMs)
}

func (s *Server) appCall(ctx context.Context, method string, params map[string]any) (any, error) {
	if s.deps.AppBridge == nil {
		return nil, fmt.Errorf("desktop backend unavailable")
	}
	return s.deps.AppBridge.Call(ctx, method, params)
}

func (s *Server) evaluate(ctx context.Context, expression string, await bool, contextID *int, timeoutMs int) (any, error) {
	value, _, exception, err := s.evaluateDetail(ctx, expression, await, contextID, timeoutMs)
	if err != nil {
		return nil, err
	}
	if exception != "" {
		return nil, fmt.Errorf("%s", exception)
	}
	return value, nil
}

func (s *Server) evaluateDetail(ctx context.Context, expression string, await bool, contextID *int, timeoutMs int) (any, string, string, error) {
	value, valueType, exception, err := s.evaluateOnce(ctx, expression, await, contextID, timeoutMs)
	// WMPF 把 Runtime.evaluate 的表达式按「表达式位置」编译：语句级输入
	// （var/function 声明、分号）直接抛 SyntaxError——实测嵌套函数声明必炸，
	// 而展平的 IIFE 正常。已经写成 IIFE 的不重试（双重包装会把返回值吃成
	// undefined）；其余用 IIFE 重试一次，取值需要显式 return，工具描述已写明。
	if err == nil && exception != "" && strings.Contains(exception, "SyntaxError") && !iifePrefixed(expression) {
		wrapper := "(function(){\n%s\n})()"
		if await {
			wrapper = "(async function(){\n%s\n})()"
		}
		value, valueType, exception, err = s.evaluateOnce(ctx, fmt.Sprintf(wrapper, expression), await, contextID, timeoutMs)
	}
	// 超时的命令早已发给小程序（副作用已发生），迟到响应在 Core 桥被静默丢弃
	// ——只回失败会把 agent 引去怀疑「没写进去」，所以把这一事实写进错误。
	if err != nil && strings.Contains(err.Error(), "timed out") {
		return value, valueType, exception, fmt.Errorf("%s (the command was dispatched and may have taken effect - late responses are dropped)", err)
	}
	return value, valueType, exception, err
}

// iifePrefixed reports whether the expression already opens as an IIFE, which
// the SyntaxError retry must not double-wrap.
func iifePrefixed(expression string) bool {
	trimmed := strings.TrimSpace(expression)
	for _, prefix := range []string{"(function", "(async", "( async", "async function"} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

func (s *Server) evaluateOnce(ctx context.Context, expression string, await bool, contextID *int, timeoutMs int) (any, string, string, error) {
	params := map[string]any{"expression": expression, "returnByValue": true}
	if await {
		params["awaitPromise"] = true
	}
	if contextID != nil {
		params["contextId"] = *contextID
	}
	resp, err := s.cdp(ctx, "Runtime.evaluate", params, timeoutMs)
	if err != nil {
		return nil, "", "", err
	}
	// Core 桥会把 WMPF 的 {id, error:{...}} 回执 reject 成 err；这一层兜底
	// 防的是旧 Core 或直连响应仍以 error 信封出现在 result 位置的情况——
	// 不查它，对不存在 context 的求值就会回 {"type":"","value":null}，
	// 与真空返回无法区分。
	if errEnvelope, ok := nested(resp, "error").(map[string]any); ok {
		message, _ := errEnvelope["message"].(string)
		if message == "" {
			message = fmt.Sprintf("CDP error: %v", errEnvelope)
		}
		return nil, "", "", fmt.Errorf("%s", message)
	}
	result, _ := nested(resp, "result").(map[string]any)
	if details, ok := result["exceptionDetails"].(map[string]any); ok {
		description, _ := nested(details, "exception", "description").(string)
		if description == "" {
			description, _ = details["text"].(string)
		}
		return nil, "undefined", description, nil
	}
	inner, _ := result["result"].(map[string]any)
	valueType, _ := inner["type"].(string)
	return inner["value"], valueType, "", nil
}

// a11yNodeCap / a11yTextCap / a11yDepthCap bound the accessibility tree walk.
const (
	a11yNodeCap  = 300
	a11yTextCap  = 64
	a11yDepthCap = 18
)

// a11yTree walks the current page webview's DOM: per node the tag, trimmed
// leaf text and bounding rect, addressed by a stable sibling-index path
// ("0/2/1") so miniapp_click {node_path} re-resolves it statelessly. Event
// names are deliberately absent for now: the exparser bridge that would expose
// them is still being verified live, and a guessed API would lie about what a
// node handles — click {node_path} already dispatches exparser tap when the
// bridge answers.
func (s *Server) a11yTree(ctx context.Context) (any, error) {
	contextID, err := s.pageContextID(ctx)
	if err != nil {
		return nil, err
	}
	expression := `(function(){var MAXN=` + strconv.Itoa(a11yNodeCap) + `,MAXT=` + strconv.Itoa(a11yTextCap) + `,MAXD=` + strconv.Itoa(a11yDepthCap) + `;var out=[],truncated=false;function childPath(path,i){return path===''?String(i):path+'/'+i}function rect(el){var r=el.getBoundingClientRect();return [Math.round(r.left),Math.round(r.top),Math.round(r.width),Math.round(r.height)]}function walk(el,path,depth){var kids=el.children||[];for(var i=0;i<kids.length;i++){if(out.length>=MAXN||depth>MAXD){truncated=true;return}var c=kids[i];var tag=(c.tagName||'').toLowerCase();if(tag==='script'||tag==='style'){continue}var p=childPath(path,i);var node={path:p,tag:tag,rect:rect(c)};if(c.childElementCount===0){var text=(c.textContent||'').replace(/\s+/g,' ').trim();if(text)node.text=text.slice(0,MAXT)}out.push(node);walk(c,p,depth+1)}}if(document.body){walk(document.body,'',0)}return JSON.stringify({route:(document.body&&document.body.getAttribute("is"))||"",nodes:out,count:out.length,truncated:truncated})})()`
	return s.evaluateJSON(ctx, expression, false, contextID, 10000)
}

// callSummaryDepth bounds how deep the call-result summary walks;
// callSummaryKeys bounds how wide one object is summarized (keys sorted for a
// deterministic reply).
const (
	callSummaryDepth = 4
	callSummaryKeys  = 64
)

// summarizeCallValue renders a call result as its shape: leaves become
// type+length (a number with ten or more digits becomes just its digit count),
// objects keep key names with summarized values, arrays their length plus the
// first three items, sensitive keys collapse entirely. Depth 4, width 64 keys
// — beyond either only the container size. The point: field values — tokens
// included — never leave the miniapp; the agent steers by the shape alone.
func summarizeCallValue(value any, depth int) any {
	switch typed := value.(type) {
	case map[string]any:
		if depth >= callSummaryDepth {
			return map[string]any{"type": "object", "keys": len(typed)}
		}
		names := make([]string, 0, len(typed))
		for key := range typed {
			names = append(names, key)
		}
		sort.Strings(names)
		out := make(map[string]any, len(names))
		shown := names
		if len(shown) > callSummaryKeys {
			shown = shown[:callSummaryKeys]
		}
		for _, key := range shown {
			if sensitiveKey(key) {
				out[key] = redactSensitiveValue(typed[key])
				continue
			}
			out[key] = summarizeCallValue(typed[key], depth+1)
		}
		if omitted := len(names) - len(shown); omitted > 0 {
			out["…keys_omitted"] = omitted
		}
		return out
	case []any:
		summary := map[string]any{"type": "array", "len": len(typed)}
		if depth < callSummaryDepth {
			preview := typed
			if len(preview) > 3 {
				preview = preview[:3]
			}
			items := make([]any, 0, len(preview))
			for _, item := range preview {
				items = append(items, summarizeCallValue(item, depth+1))
			}
			summary["items"] = items
		}
		return summary
	case string:
		return map[string]any{"type": "string", "len": len([]rune(typed))}
	case float64:
		digits := len(strings.Trim(strings.Trim(strconv.FormatFloat(typed, 'f', -1, 64), "-"), "."))
		if digits >= 10 {
			return map[string]any{"type": "number", "digits": digits}
		}
		return typed
	case bool, nil:
		return typed
	default:
		return map[string]any{"type": fmt.Sprintf("%T", value)}
	}
}

// navigationVerifyTimeout / navigationVerifyPoll shape the post-dispatch
// verification window; vars so tests can shrink the real-clock wait.
var (
	navigationVerifyTimeout = 5 * time.Second
	navigationVerifyPoll    = 300 * time.Millisecond
)

// stackShape reads the current page stack depth and top route. A failed read
// is (0, "") — navigateBack verification then cannot prove a depth change and
// accepts the first successful stack read instead.
func (s *Server) stackShape(ctx context.Context) (int, string) {
	stack, err := s.appCall(ctx, "navigator.pageStack", map[string]any{})
	if err != nil {
		return 0, ""
	}
	layers := anySlice(objectMap(stack)["stack"])
	if len(layers) == 0 {
		return 0, ""
	}
	top, _ := objectMap(layers[len(layers)-1])["route"].(string)
	return len(layers), top
}

// verifyNavigation polls the real page stack until the navigation lands:
// forward navigations must show the expected route on top, navigateBack must
// shrink the stack by delta. A fresh layer can read "" while its webview is
// still mounting (observed live) — the page webview's body[is] fills it in
// before we declare failure. The wx success callback alone is not evidence:
// live testing saw ok answers with the stack unchanged and with empty routes.
func (s *Server) verifyNavigation(ctx context.Context, method, expected string, delta, beforeDepth int, beforeTop string) (string, error) {
	deadline := time.Now().Add(navigationVerifyTimeout)
	observed := ""
	bodyRouteDone := false
	for {
		stack, err := s.appCall(ctx, "navigator.pageStack", map[string]any{})
		if err == nil {
			layers := anySlice(objectMap(stack)["stack"])
			depth := len(layers)
			if depth > 0 {
				observed, _ = objectMap(layers[depth-1])["route"].(string)
				// 空路由只扫一次 body[is]（每次核验最多 24 个探测，别随轮询翻倍）。
				if observed == "" && !bodyRouteDone {
					observed = s.bodyRoute(ctx)
					bodyRouteDone = true
				}
			}
			if method == "navigateBack" {
				if beforeDepth == 0 {
					// 分发前的栈读不到，深度收敛无从谈起——分发成功即认。
					return observed, nil
				}
				// 深度真的减了 delta 即为落地。不看栈顶路由：相同页面连压两次
				// 再回退时栈顶路由不变，拿它比对会把真落地误判成假 ok。
				if depth > 0 && depth <= beforeDepth-delta {
					return observed, nil
				}
			} else if observed != "" && observed == expected {
				return observed, nil
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(navigationVerifyPoll)
	}
	if method == "navigateBack" {
		return observed, fmt.Errorf("navigation did not land: stack depth did not shrink by %d within %s (top layer: %q)", delta, navigationVerifyTimeout, observed)
	}
	return observed, fmt.Errorf("navigation did not land: expected %q on top of the stack within %s, got %q", expected, navigationVerifyTimeout, observed)
}

// bodyRoute scans the page webviews for a body[is] marker and returns the
// newest one — used to fill in a stack layer whose route still reads "" while
// the webview has already mounted. Context ids are handed out in creation
// order, so the scan goes from the highest id down: the newest webview is the
// one the top stack layer belongs to; a lower id's route would be a background
// page's. Probes are bounded (1s each) and stop at the first hit. The
// attribute is read with double quotes on purpose: the single-quoted form
// collides with the context resolver's probe marker and would read its canned
// "no" as a route.
func (s *Server) bodyRoute(ctx context.Context) string {
	for id := contextProbeMaxID; id >= 1; id-- {
		contextID := id
		value, err := s.evaluateJSON(ctx, `(function(){return (document.body&&document.body.getAttribute("is"))||""})()`, false, &contextID, 1000)
		if err != nil {
			continue
		}
		if route, ok := value.(string); ok && route != "" {
			return route
		}
	}
	return ""
}

// navigateBackInPageWebview retries navigateBack inside the current page's
// webview: a foreground plugin page refuses appservice-side dispatches
// ("rejected due to no permission currently", observed live), while the page
// webview's own wx.navigateBack goes through.
func (s *Server) navigateBackInPageWebview(ctx context.Context, delta int) error {
	contextID, err := s.pageContextID(ctx)
	if err != nil {
		return err
	}
	_, err = s.evaluate(ctx, fmt.Sprintf(`new Promise(function(r){wx.navigateBack({delta:%d,complete:r})})`, delta), true, contextID, 5000)
	return err
}

// screenshotNote attaches this capture's route and viewport: the screenshot
// goes to the locked connection's browser-level endpoint and can disagree with
// the page stack (stack says page A, image shows page B — observed live). The
// metadata rides with the image so the agent can self-check what it captured.
func (s *Server) screenshotNote(ctx context.Context) string {
	var parts []string
	if route, err := s.appCall(ctx, "navigator.currentRoute", map[string]any{}); err == nil {
		if value, _ := objectMap(route)["route"].(string); value != "" {
			parts = append(parts, "route:"+value)
		}
	}
	if contextID, err := s.pageContextID(ctx); err == nil {
		value, err := s.evaluateJSON(ctx, `(function(){return JSON.stringify({w:window.innerWidth,h:window.innerHeight,dpr:window.devicePixelRatio||1})})()`, false, contextID, 3000)
		if err == nil {
			view := objectMap(value)
			parts = append(parts, fmt.Sprintf("viewport:%gx%g dpr:%g", view["w"], view["h"], view["dpr"]))
		}
	}
	return strings.Join(parts, " ")
}

func (s *Server) evaluateJSON(ctx context.Context, expression string, await bool, contextID *int, timeoutMs int) (any, error) {
	value, err := s.evaluate(ctx, expression, await, contextID, timeoutMs)
	if err != nil {
		return nil, err
	}
	text, ok := value.(string)
	if !ok {
		return value, nil
	}
	var parsed any
	if json.Unmarshal([]byte(text), &parsed) != nil {
		return value, nil
	}
	return parsed, nil
}

// evaluateStorage runs a wx.* storage expression in the mini program's logic
// layer, converting a page-side {error} into a tool error on the way out so
// both the reads and the writes fail loudly. Storage is owned there: the host
// page has no wx, and a page webview's wx is the render-layer object.
func (s *Server) evaluateStorage(ctx context.Context, expression string, timeoutMs int) (any, error) {
	contextID, err := s.appServiceContextID(ctx)
	if err != nil {
		return nil, err
	}
	result, err := s.evaluateJSON(ctx, expression, false, contextID, timeoutMs)
	return inlineErrorResult(result, err)
}

// inlineErrorResult 把页内 try/catch 的 {error} 转成工具错误。页内助手一律用
// try/catch 把失败当数据回，直接把它当成功转发出去，agent 看到的会是「成功了，
// 顺便带个 error 字段」——一次失败的读会读成一次空的读。
func inlineErrorResult(result any, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	if message := stringArg(objectMap(result), "error", ""); message != "" {
		return nil, fmt.Errorf("%s", message)
	}
	return result, nil
}

func httpTool(ctx context.Context, args map[string]any) (any, error) {
	method := strings.ToUpper(stringArg(args, "method", "GET"))
	body := strings.NewReader(stringArg(args, "body", ""))
	req, err := http.NewRequestWithContext(ctx, method, stringArg(args, "url", ""), body)
	if err != nil {
		return nil, err
	}
	if headers, ok := args["headers"].(map[string]any); ok {
		for key, value := range headers {
			req.Header.Set(key, fmt.Sprint(value))
		}
	}
	timeoutSeconds := numberArgDefault(args, "timeout", 15)
	if timeoutSeconds <= 0 {
		timeoutSeconds = 15
	}
	if timeoutSeconds > 120 {
		timeoutSeconds = 120
	}
	timeout := time.Duration(timeoutSeconds * float64(time.Second))
	// The debug console must reach user-supplied endpoints, including local and
	// dev endpoints with self-signed certificates. Verification stays off by design.
	//nolint:gosec // G402: intentional, the request target is chosen by the operator.
	client := &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 50001))
	if err != nil {
		return nil, err
	}
	if len(data) > 50000 {
		data = data[:50000]
	}
	headers := map[string]string{}
	for key, values := range resp.Header {
		headers[key] = strings.Join(values, ", ")
	}
	return map[string]any{"status": resp.StatusCode, "headers": headers, "body": string(data), "elapsed_ms": time.Since(start).Milliseconds()}, nil
}

func readSkills(dir string) (any, error) {
	if dir == "" {
		return map[string]any{"message": "暂无 skill 文件。", "skills_dir": dir}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return map[string]any{"message": "暂无 skill 文件。", "skills_dir": dir}, nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	parts := []string{}
	for _, entry := range entries {
		if entry.IsDir() || strings.EqualFold(entry.Name(), "README.md") || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err == nil {
			parts = append(parts, "## ["+entry.Name()+"]\n\n"+string(data))
		}
	}
	if len(parts) == 0 {
		return map[string]any{"message": "暂无 skill 文件。", "skills_dir": dir}, nil
	}
	return map[string]any{"skills": strings.Join(parts, "\n\n---\n\n"), "skills_dir": dir}, nil
}

func stringArg(args map[string]any, key, fallback string) string {
	if value, ok := args[key].(string); ok {
		return value
	}
	return fallback
}

func boolArg(args map[string]any, key string) bool {
	value, _ := args[key].(bool)
	return value
}

func numberArg(args map[string]any, key string) (float64, bool) {
	value, ok := args[key].(float64)
	return value, ok
}

func numberArgDefault(args map[string]any, key string, fallback float64) float64 {
	if value, ok := numberArg(args, key); ok {
		return value
	}
	return fallback
}

func intArg(args map[string]any, key string, fallback int) int {
	if value, ok := numberArg(args, key); ok {
		return int(value)
	}
	return fallback
}

func nested(value any, keys ...string) any {
	current := value
	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[key]
	}
	return current
}

func objectMap(value any) map[string]any {
	if result, ok := value.(map[string]any); ok {
		return result
	}
	data, err := json.Marshal(value)
	if err != nil {
		return map[string]any{}
	}
	result := map[string]any{}
	if json.Unmarshal(data, &result) != nil {
		return map[string]any{}
	}
	return result
}

func anySlice(value any) []any {
	if items, ok := value.([]any); ok {
		return items
	}
	data, err := json.Marshal(value)
	if err == nil {
		items := []any{}
		if json.Unmarshal(data, &items) == nil {
			return items
		}
	}
	return []any{}
}

func mapKeys(values map[string]int) []string {
	keys := make([]string, 0, len(values))
	for key, count := range values {
		if count > 0 {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func jsLiteral(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

// ensureWithinCodeRoot pins code browsing paths to the decompiled output
// tree: the agent-facing tools promise to read decompiled sources, and the
// audited package code itself is untrusted input that must not be able to
// steer a file read outside that tree.
func (s *Server) ensureWithinCodeRoot(path string) error {
	root := s.deps.AllowedCodeRoot
	if root == "" || path == "" {
		return errors.New("path must be within the decompiled output directory")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("path is not usable: %w", err)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("output root is not usable: %w", err)
	}
	rel, err := filepath.Rel(rootAbs, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("path must be within the decompiled output directory: " + root)
	}
	return nil
}

// scanFindingsCap bounds the findings a scan tool returns to one call. The
// scanner reports every hit per file, and minified one-line bundles routinely
// match dozens of times per rule — an unshaped report of a mid-size package
// measured at 600KB+, which is an agent context, not a result.
const scanFindingsCap = 60

// scanSnippetWindow is how many bytes of context the shaped snippet keeps on
// each side of the match column.
const scanSnippetWindow = 300

// cloudCapturesCap bounds miniapp_cloud_captures' snapshot page. The tool has
// no cursor of its own; anything past the cap is pointed at hook_drain's
// cursor paging instead of being streamed.
const cloudCapturesCap = 200

// Page-side results are unbounded by nature (an evaluate can return a whole
// dataset, storage can hold megabyte blobs); the MCP boundary caps what it
// will stream into a caller's context. Oversized answers come back truncated
// with the true size and a pointer to narrower queries.
const (
	evaluateResultCap = 48 << 10
	storageResultCap  = 32 << 10
)

// oversizedResult reports whether value, serialized, exceeds capBytes — and if
// so returns the capped shape to return instead: the head of the JSON with an
// ellipsis, the true total, and the note steering the caller to narrower reads.
func oversizedResult(value any, capBytes int) (map[string]any, bool) {
	data, err := json.Marshal(value)
	if err != nil || len(data) <= capBytes {
		return nil, false
	}
	text := string(data[:capBytes])
	for i := 0; i < utf8.UTFMax && !utf8.ValidString(text); i++ {
		text = text[:len(text)-1]
	}
	return map[string]any{
		"value":       text + "…",
		"type":        "string",
		"truncated":   true,
		"total_bytes": len(data),
		"note":        "结果超出 MCP 输出上限已截断：用更窄的表达式、单键查询或字段投影取回需要的部分",
	}, true
}

// shapeScanFindings dedups, windows and caps a scanner findings array:
//   - dedup by rule_id+value+file: the same constant matched N times in one
//     minified line is one finding for a human or an agent, not N;
//   - the snippet is re-centred on the match column — the raw snippet starts
//     at the line's first byte, which on a minified line is never where the
//     match is;
//   - everything past the cap is summarized as total/truncated instead of
//     being streamed.
func shapeScanFindings(raw any) (findings []any, total int, truncated bool) {
	all := anySlice(raw)
	if all == nil {
		return []any{}, 0, false
	}
	seen := map[string]bool{}
	findings = make([]any, 0, len(all))
	for _, item := range all {
		entry := objectMap(item)
		key := stringArg(entry, "rule_id", "") + "\x00" + stringArg(entry, "value", "") + "\x00" + stringArg(entry, "file", "")
		if seen[key] {
			continue
		}
		seen[key] = true
		total++
		if len(findings) >= scanFindingsCap {
			truncated = true
			continue
		}
		entry["snippet"] = windowSnippet(stringArg(entry, "snippet", ""), intArg(entry, "column", 0))
		findings = append(findings, entry)
	}
	if total > len(findings) {
		truncated = true
	}
	return findings, total, truncated
}

// windowSnippet keeps scanSnippetWindow bytes on each side of the 1-based
// match column, with ellipsis markers where content was cut. It trims back to
// UTF-8 character boundaries the same way miniapp_read_file does, and falls
// back to the head of an overlong snippet when no column is known.
func windowSnippet(snippet string, column int) string {
	if len(snippet) <= scanSnippetWindow*2 {
		return snippet
	}
	center := column - 1
	if center <= 0 || center > len(snippet) {
		center = 0
	}
	from := center - scanSnippetWindow
	if from < 0 {
		from = 0
	}
	to := center + scanSnippetWindow
	if to > len(snippet) {
		to = len(snippet)
	}
	text := snippet[from:to]
	for i := 0; i < utf8.UTFMax && len(text) > 0 && !utf8.ValidString(text); i++ {
		text = text[:len(text)-1]
	}
	if from > 0 {
		text = "…" + text
	}
	if to < len(snippet) {
		text += "…"
	}
	return text
}
