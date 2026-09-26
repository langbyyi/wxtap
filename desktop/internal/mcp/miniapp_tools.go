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
	"strings"
	"time"
	"unicode/utf8"
)

type imageResult struct {
	Data     string
	MimeType string
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
		return imageResult{Data: data, MimeType: mime}, nil
	case "miniapp_click":
		if selector := stringArg(args, "selector", ""); selector != "" {
			// 小程序的 DOM 只存在于当前页 webview。宿主页里根本没有小程序元素，
			// 在默认上下文里 querySelector 只会回「Element not found」。
			contextID, err := s.pageContextID(ctx)
			if err != nil {
				return nil, err
			}
			expression := `(function(){var el=document.querySelector(` + jsLiteral(selector) + `);if(!el)return JSON.stringify({error:'Element not found'});var r=el.getBoundingClientRect();el.click();return JSON.stringify({ok:true,x:r.left+r.width/2,y:r.top+r.height/2,tag:el.tagName})})()`
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
		// 一律经页面侧导航钩子，由它解析 wxFrame 并区分 tabBar 页。以前 switchTab
		// 单独向 CDP 求值 `wx.switchTab`：既落在没有 wx 的宿主页上下文（必然回
		// "wx is not defined"），又绕开了钩子，于是守卫开启时连工具自己的跳转都会
		// 被自己的守卫拦下、并且假报成功。
		_, err := s.appCall(ctx, "navigator.navigate", map[string]any{"route": route, "method": method})
		return map[string]any{"ok": err == nil, "route": route, "method": method}, err
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
		_, _ = s.cdp(ctx, "Runtime.enable", map[string]any{}, 5000)
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
		return map[string]any{"contexts": contexts, "count": len(contexts)}, nil
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
		logs := make([]any, 0, len(records))
		for _, item := range records {
			entry := objectMap(item)
			if record, ok := entry["record"].(map[string]any); ok {
				logs = append(logs, record)
				continue
			}
			logs = append(logs, item)
		}
		return map[string]any{"logs": logs, "count": len(logs)}, nil
	case "miniapp_get_info":
		result, err := s.appCall(ctx, "navigator.pages", map[string]any{})
		if err != nil {
			return nil, err
		}
		data := objectMap(result)
		data["pages_count"] = len(anySlice(data["pages"]))
		return data, nil
	case "miniapp_page_stack":
		return s.pageSnapshot(ctx)
	case "miniapp_get_storage":
		// key 缺省 = 全量（合并旧 miniapp_get_storage_key 的单键形态）。
		if key := stringArg(args, "key", ""); key != "" {
			result, err := s.evaluateStorage(ctx, `(function(){try{var v=wx.getStorageSync(`+jsLiteral(key)+`);return JSON.stringify({key:`+jsLiteral(key)+`,value:v,type:typeof v})}catch(e){return JSON.stringify({error:e.message})}})()`, 5000)
			if err != nil {
				return nil, err
			}
			if capped, over := oversizedResult(result, storageResultCap); over {
				return capped, nil
			}
			return result, nil
		}
		result, err := s.evaluateStorage(ctx, `(function(){try{var i=wx.getStorageInfoSync(),r={},k=i.keys||[];for(var n=0;n<k.length;n++){try{r[k[n]]=wx.getStorageSync(k[n])}catch(e){}}return JSON.stringify({keys:k,data:r,currentSize:i.currentSize||0})}catch(e){return JSON.stringify({error:e.message})}})()`, 8000)
		if err != nil {
			return nil, err
		}
		if capped, over := oversizedResult(result, storageResultCap); over {
			return capped, nil
		}
		return result, nil
	case "miniapp_get_storage_key":
		key := stringArg(args, "key", "")
		result, err := s.evaluateStorage(ctx, `(function(){try{var v=wx.getStorageSync(`+jsLiteral(key)+`);return JSON.stringify({key:`+jsLiteral(key)+`,value:v,type:typeof v})}catch(e){return JSON.stringify({error:e.message})}})()`, 5000)
		if err != nil {
			return nil, err
		}
		// 合并形态 miniapp_get_storage 的单键读有同一顶 32KB 帽，旧名不得成为
		// 绕开它的口子。
		if capped, over := oversizedResult(result, storageResultCap); over {
			return capped, nil
		}
		return result, nil
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
		if boolArg(args, "clear") {
			_ = s.deps.Core.HookClear(ctx, "cloud")
		}
		result := map[string]any{"captures": captures, "count": len(captures), "truncated": truncated}
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
		return map[string]any{"output_dir": outputDir, "files_count": filesCount, "packages_processed": packagesProcessed}, nil
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
		scan, err := s.deps.Scan(ctx, scanPath)
		if err != nil {
			return nil, err
		}
		report := map[string]any{}
		_ = json.Unmarshal(scan.Report, &report)
		findings, total, truncated := shapeScanFindings(report["findings"])
		analysis, _ := report["result"].(map[string]any)
		if analysis == nil {
			analysis = map[string]any{}
		}
		return map[string]any{
			"scan_path": scanPath, "files_scanned": scan.FilesScanned,
			"findings": findings, "total_findings": total, "truncated": truncated,
			"result":           analysis,
			"categories_found": mapKeys(scan.Summary),
		}, nil
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
func (s *Server) pageSnapshot(ctx context.Context) (any, error) {
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
		out["stack"] = data["stack"]
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
