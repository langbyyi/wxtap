// Package wxopen calls the official WeChat open-platform endpoints that show
// what a discovered AppID/AppSecret can actually reach: it turns "this
// credential is valid" (ak.Verify) into "this credential reads or creates the
// following resources".
//
// Every path, method and parameter below was checked against the official
// documentation (2026-09-23). Sources, one per family:
//
//   - mini:    https://developers.weixin.qq.com/miniprogram/dev/OpenApiDoc/
//   - oa:      https://developers.weixin.qq.com/doc/offiaccount/
//   - work:    https://developer.work.weixin.qq.com/document/path/
//
// A new endpoint is expected to cite the same kind of source, because a wrong
// path is only visible at runtime as an errcode.
//
// Boundaries, kept deliberately:
//
//   - Every call is triggered explicitly by the caller; the package never
//     enumerates or probes on its own.
//   - Only reading or inspection endpoints are listed, plus the three mini
//     program code generators that the deep-link tests need. Message-sending
//     endpoints (template / subscribe / customer-service / webhook) are absent
//     on purpose: they deliver to real users, which is not a debugging tool's
//     remit.
//   - The access token stays in this process's memory, keyed by credential
//     identity, and is never written to disk, logged, or echoed to the caller.
package wxopen

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/ak"
)

const (
	defaultTimeout = 20 * time.Second
	defaultBodyCap = 4 << 20
	// tokenExpirySkew refreshes a cached token slightly early so a call never
	// races the server-side expiry.
	tokenExpirySkew = 60 * time.Second
)

// Param describes one endpoint parameter the UI renders as an input.
type Param struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Kind     string `json:"kind"` // string | number | bool | json
	Required bool   `json:"required,omitempty"`
	Hint     string `json:"hint,omitempty"`
	Default  string `json:"default,omitempty"`
	// Shared marks a parameter several endpoints of a family reuse (an openid,
	// a page cursor). The console renders those once in 动态参数 instead of
	// asking for the same value on every endpoint.
	Shared bool `json:"shared,omitempty"`
}

// Endpoint is one callable official endpoint. BaseURL is fixed per group, so a
// request can never be pointed at an arbitrary host.
type Endpoint struct {
	ID    string `json:"id"`
	Group string `json:"group"` // mini | oa | work
	Label string `json:"label"`
	// Category groups endpoints in the UI, so the grouping is the backend's
	// decision rather than something the console invents per family.
	Category string  `json:"category"`
	Summary  string  `json:"summary"`
	Method   string  `json:"method"`
	Path     string  `json:"path"`
	JSON     bool    `json:"json_body"` // params travel as a JSON body, not a query
	Returns  string  `json:"returns"`   // json | image
	Note     string  `json:"note,omitempty"`
	Params   []Param `json:"params,omitempty"`
}

// Group is a credential family the endpoints belong to.
type Group struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Request is one explicit endpoint call.
type Request struct {
	Mode      string            `json:"mode"`
	AccessKey string            `json:"access_key"`
	SecretKey string            `json:"secret_key"`
	Endpoint  string            `json:"endpoint"`
	Params    map[string]string `json:"params"`
}

// Result is what the UI shows. URL and Body are the exchange as it happened,
// token included: a masked packet is not the packet.
type Result struct {
	Endpoint     string `json:"endpoint"`
	Label        string `json:"label"`
	Method       string `json:"method"`
	URL          string `json:"url"`
	HTTPStatus   int    `json:"http_status"`
	ErrCode      int    `json:"errcode"`
	ErrMsg       string `json:"errmsg"`
	DurationMs   int64  `json:"duration_ms"`
	Body         any    `json:"body,omitempty"`
	ImageDataURL string `json:"image_data_url,omitempty"`
	// 完整的 HTTP 交换：右侧按报文格式展示请求与响应两段，而不是只给一个
	// 结果体。URL 里的 token 与结果体一样按原样回显。
	RequestHeaders  map[string]string `json:"request_headers,omitempty"`
	RequestBody     string            `json:"request_body,omitempty"`
	ResponseHeaders map[string]string `json:"response_headers,omitempty"`
}

// TokenSource resolves an access token for one credential. It is a seam so the
// endpoint logic can be tested without the official service.
type TokenSource func(ctx context.Context, mode, accessKey, secretKey string) (token string, expiresIn int, err error)

// Client performs endpoint calls. The zero value is not usable; use Default or
// build one with a token source.
type Client struct {
	HTTP  *http.Client
	Token TokenSource
	// BaseURL overrides the official host, for tests only.
	BaseURL string
	Now     func() time.Time

	mu    sync.Mutex
	cache map[string]cachedToken
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// Default is the production client: official hosts, ak-based token lookups.
var Default = &Client{Token: akTokenSource}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: defaultTimeout}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) baseURL(group string) string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	if group == "work" {
		return "https://qyapi.weixin.qq.com"
	}
	return "https://api.weixin.qq.com"
}

// Token returns a cached token when one is still valid, otherwise resolves a
// fresh one. The cache is in memory only and keyed by credential identity, so
// switching accounts never reuses another account's token.
func (c *Client) tokenFor(ctx context.Context, mode, accessKey, secretKey string) (string, error) {
	key := mode + "\x00" + accessKey
	c.mu.Lock()
	if entry, ok := c.cache[key]; ok && c.now().Before(entry.expiresAt) {
		c.mu.Unlock()
		return entry.token, nil
	}
	c.mu.Unlock()

	if c.Token == nil {
		return "", fmt.Errorf("未配置 token 来源")
	}
	token, expiresIn, err := c.Token(ctx, mode, accessKey, secretKey)
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", fmt.Errorf("凭据未换到 access_token")
	}
	lifetime := time.Duration(expiresIn) * time.Second
	if lifetime <= tokenExpirySkew {
		lifetime = tokenExpirySkew + time.Minute
	}
	c.mu.Lock()
	if c.cache == nil {
		c.cache = map[string]cachedToken{}
	}
	c.cache[key] = cachedToken{token: token, expiresAt: c.now().Add(lifetime - tokenExpirySkew)}
	c.mu.Unlock()
	return token, nil
}

// Call performs one endpoint call and returns a display-ready result.
func (c *Client) Call(ctx context.Context, request Request) (Result, error) {
	endpoint, ok := lookup(request.Endpoint)
	if !ok {
		return Result{}, fmt.Errorf("未知接口: %s", request.Endpoint)
	}
	mode := strings.TrimSpace(request.Mode)
	if mode != endpoint.Group {
		return Result{}, fmt.Errorf("%s 只适用于%s凭据", endpoint.Label, groupLabel(endpoint.Group))
	}
	if strings.TrimSpace(request.AccessKey) == "" || strings.TrimSpace(request.SecretKey) == "" {
		return Result{}, fmt.Errorf("请先填写凭据")
	}

	token, err := c.tokenFor(ctx, mode, request.AccessKey, request.SecretKey)
	if err != nil {
		return Result{}, err
	}

	base := c.baseURL(endpoint.Group)
	realQuery := url.Values{"access_token": {token}}
	var bodyReader io.Reader
	requestBody := ""
	if endpoint.JSON {
		payload, err := jsonBody(endpoint, request.Params)
		if err != nil {
			return Result{}, err
		}
		requestBody = payload
		bodyReader = strings.NewReader(payload)
	} else {
		for id, value := range request.Params {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				realQuery.Set(id, trimmed)
			}
		}
	}
	realURL := base + endpoint.Path + "?" + realQuery.Encode()

	req, err := http.NewRequestWithContext(ctx, endpoint.Method, realURL, bodyReader)
	if err != nil {
		return Result{}, err
	}
	if endpoint.JSON {
		req.Header.Set("Content-Type", "application/json")
	}

	started := c.now()
	response, err := c.client().Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("请求失败: %w", sanitizeURLError(err))
	}
	defer func() { _ = response.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(response.Body, defaultBodyCap+1))
	if err != nil {
		return Result{}, fmt.Errorf("读取响应失败: %w", err)
	}
	if len(data) > defaultBodyCap {
		return Result{}, fmt.Errorf("响应超过 %d 字节", defaultBodyCap)
	}

	result := Result{
		Endpoint:        endpoint.ID,
		Label:           endpoint.Label,
		Method:          endpoint.Method,
		URL:             realURL,
		HTTPStatus:      response.StatusCode,
		DurationMs:      c.now().Sub(started).Milliseconds(),
		RequestHeaders:  headerMap(req.Header),
		RequestBody:     requestBody,
		ResponseHeaders: headerMap(response.Header),
	}
	if strings.HasPrefix(response.Header.Get("Content-Type"), "image/") {
		result.ImageDataURL = dataURL(response.Header.Get("Content-Type"), data)
		return result, nil
	}
	var payload any
	if json.Unmarshal(data, &payload) == nil {
		result.Body = payload
	} else {
		result.Body = string(data)
	}
	if fields, ok := payload.(map[string]any); ok {
		if code, ok := fields["errcode"].(float64); ok {
			result.ErrCode = int(code)
		}
		if message, ok := fields["errmsg"].(string); ok {
			result.ErrMsg = message
		}
		result.Body = fields
	}
	return result, nil
}

// jsonBody converts the UI's string parameters into the typed JSON the
// endpoint expects, rejecting values that cannot be converted.
func jsonBody(endpoint Endpoint, params map[string]string) (string, error) {
	payload := map[string]any{}
	for _, param := range endpoint.Params {
		raw := strings.TrimSpace(params[param.ID])
		if raw == "" {
			if param.Required {
				return "", fmt.Errorf("%s 不能为空", param.Label)
			}
			if param.Default != "" {
				raw = param.Default
			} else {
				continue
			}
		}
		switch param.Kind {
		case "number":
			value, err := strconv.Atoi(raw)
			if err != nil {
				return "", fmt.Errorf("%s 需要整数: %s", param.Label, raw)
			}
			payload[param.ID] = value
		case "bool":
			value, err := strconv.ParseBool(raw)
			if err != nil {
				return "", fmt.Errorf("%s 需要 true/false: %s", param.Label, raw)
			}
			payload[param.ID] = value
		case "json":
			var value any
			if err := json.Unmarshal([]byte(raw), &value); err != nil {
				return "", fmt.Errorf("%s 不是有效 JSON: %s", param.Label, raw)
			}
			payload[param.ID] = value
		default:
			payload[param.ID] = raw
		}
	}
	for _, param := range endpoint.Params {
		if param.Required {
			if _, ok := payload[param.ID]; !ok {
				return "", fmt.Errorf("%s 不能为空", param.Label)
			}
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// headerMap flattens the headers worth showing in a packet view: multiple
// values are joined so one line per name survives.
func headerMap(headers http.Header) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(headers))
	for name, values := range headers {
		out[name] = strings.Join(values, ", ")
	}
	return out
}

func dataURL(contentType string, data []byte) string {
	mime := strings.TrimSpace(strings.Split(contentType, ";")[0])
	if mime == "" {
		mime = "image/png"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func lookup(id string) (Endpoint, bool) {
	for _, endpoint := range Endpoints() {
		if endpoint.ID == id {
			return endpoint, true
		}
	}
	return Endpoint{}, false
}

func groupLabel(group string) string {
	for _, item := range Groups() {
		if item.ID == group {
			return item.Label
		}
	}
	return group
}

// akTokenSource is the production TokenSource: it reuses ak.Verify so the
// credential check, sanitising and timeouts stay in one place.
func akTokenSource(ctx context.Context, mode, accessKey, secretKey string) (string, int, error) {
	result, err := ak.Default.Verify(ctx, ak.Request{
		Mode: mode, AccessKey: accessKey, SecretKey: secretKey, IncludeToken: true,
	})
	if err != nil {
		return "", 0, err
	}
	if !result.Valid || result.Token == "" {
		message := result.ErrMsg
		if message == "" {
			message = fmt.Sprintf("errcode %d", result.ErrCode)
		}
		return "", 0, fmt.Errorf("凭据无效: %s", message)
	}
	return result.Token, result.ExpiresIn, nil
}

// Groups lists the credential families in menu order.
func Groups() []Group {
	return []Group{
		{ID: "mini", Label: "微信小程序"},
		{ID: "oa", Label: "微信公众号"},
		{ID: "work", Label: "企业微信"},
	}
}

// Endpoints is the callable table. It is the single source of truth for both
// the IPC listing and the call path, so the UI can never call an endpoint the
// backend does not describe. Paths and parameters follow the official docs
// cited at the top of this file; only reading, generating or inspecting
// endpoints are listed.
func Endpoints() []Endpoint {
	return []Endpoint{
		// --- 微信小程序 ---
		{
			ID: "mini-code-unlimited", Group: "mini", Category: "小程序码", Label: "生成小程序码（不限量）",
			Summary: "为任意页面路径和 scene 参数生成可扫描的小程序码，用于验证站外直达与页面参数是否受限。",
			Method:  http.MethodPost, Path: "/wxa/getwxacodeunlimit", JSON: true, Returns: "image",
			Note: "check_path 为 false 时允许页面未发布或不存在（上限 60000 个页面）；page 必须与 app.json 一致且不能带参数，参数走 scene。",
			Params: []Param{
				{ID: "scene", Label: "scene", Kind: "string", Required: true, Hint: "最多 32 个可见字符，随码进入页面"},
				{ID: "page", Label: "page", Kind: "string", Hint: "如 pages/admin/index，不带前导 /、不带参数；留空进首页", Shared: true},
				{ID: "check_path", Label: "check_path", Kind: "bool", Default: "false"},
				{ID: "env_version", Label: "env_version", Kind: "string", Hint: "release | trial | develop，默认 release", Shared: true},
				{ID: "width", Label: "width", Kind: "number", Hint: "280..1280，默认 430", Shared: true},
			},
		},
		{
			ID: "mini-code-path", Group: "mini", Category: "小程序码", Label: "生成小程序码（按路径）",
			Summary: "按已发布页面路径生成小程序码，路径必须存在。",
			Method:  http.MethodPost, Path: "/wxa/getwxacode", JSON: true, Returns: "image",
			Params: []Param{
				{ID: "path", Label: "path", Kind: "string", Required: true, Hint: "如 pages/index/index"},
				{ID: "width", Label: "width", Kind: "number", Shared: true},
			},
		},
		{
			ID: "mini-qrcode", Group: "mini", Category: "小程序码", Label: "生成普通二维码",
			Summary: "生成指向小程序页面的普通二维码。",
			Method:  http.MethodPost, Path: "/cgi-bin/wxaapp/createwxaqrcode", JSON: true, Returns: "image",
			Params: []Param{
				{ID: "path", Label: "path", Kind: "string", Required: true},
				{ID: "width", Label: "width", Kind: "number", Shared: true},
			},
		},
		{
			ID: "mini-sec-check", Group: "mini", Category: "内容安全", Label: "内容安全检测",
			Summary: "提交文本到内容安全接口，确认服务端检测能力与拦截口径。",
			Method:  http.MethodPost, Path: "/wxa/msg_sec_check", JSON: true, Returns: "json",
			Note: "V2 接口：openid 必填，且该用户需在近两小时内访问过小程序，否则返回 61010。",
			Params: []Param{
				{ID: "content", Label: "content", Kind: "string", Required: true},
				{ID: "version", Label: "version", Kind: "number", Default: "2"},
				{ID: "scene", Label: "scene", Kind: "number", Default: "1", Hint: "1 资料 2 评论 3 论坛 4 社交日志"},
				{ID: "openid", Label: "openid", Kind: "string", Required: true, Hint: "近两小时内访问过小程序的用户 openid", Shared: true},
			},
		},
		{
			ID: "mini-account-info", Group: "mini", Category: "账号信息", Label: "小程序基本信息",
			Summary: "读取小程序的名称、主体、类目等账号信息。",
			Method:  http.MethodPost, Path: "/cgi-bin/account/getaccountbasicinfo", JSON: true, Returns: "json",
		},
		{
			ID: "mini-summary-trend", Group: "mini", Category: "数据分析", Label: "每日概况趋势",
			Summary: "读取核心指标（打开次数、访问人数等）的每日趋势。",
			Method:  http.MethodPost, Path: "/datacube/getweanalysisappiddailysummarytrend", JSON: true, Returns: "json",
			Params: []Param{
				{ID: "begin_date", Label: "begin_date", Kind: "string", Required: true, Hint: "20260901 形式，最长 30 天跨度", Shared: true},
				{ID: "end_date", Label: "end_date", Kind: "string", Required: true, Shared: true},
			},
		},
		{
			ID: "mini-visit-page", Group: "mini", Category: "数据分析", Label: "访问页面",
			Summary: "读取各页面的访问次数与人数，可用于确认后台页面是否被人访问过。",
			Method:  http.MethodPost, Path: "/datacube/getweanalysisappidvisitpage", JSON: true, Returns: "json",
			Params: []Param{
				{ID: "begin_date", Label: "begin_date", Kind: "string", Required: true, Shared: true},
				{ID: "end_date", Label: "end_date", Kind: "string", Required: true, Shared: true},
			},
		},

		{
			ID: "mini-daily-trend", Group: "mini", Category: "数据分析", Label: "日访问趋势",
			Summary: "读取每日打开次数与访问人数趋势。",
			Method:  http.MethodPost, Path: "/datacube/getweanalysisappiddailyvisittrend", JSON: true, Returns: "json",
			Params: []Param{
				{ID: "begin_date", Label: "begin_date", Kind: "string", Required: true, Shared: true},
				{ID: "end_date", Label: "end_date", Kind: "string", Required: true, Shared: true},
			},
		},
		{
			ID: "mini-weekly-trend", Group: "mini", Category: "数据分析", Label: "周访问趋势",
			Summary: "读取每周打开次数与访问人数趋势。",
			Method:  http.MethodPost, Path: "/datacube/getweanalysisappidweeklyvisittrend", JSON: true, Returns: "json",
			Params: []Param{
				{ID: "begin_date", Label: "begin_date", Kind: "string", Required: true, Shared: true},
				{ID: "end_date", Label: "end_date", Kind: "string", Required: true, Shared: true},
			},
		},
		{
			ID: "mini-monthly-trend", Group: "mini", Category: "数据分析", Label: "月访问趋势",
			Summary: "读取每月打开次数与访问人数趋势。",
			Method:  http.MethodPost, Path: "/datacube/getweanalysisappidmonthlyvisittrend", JSON: true, Returns: "json",
			Params: []Param{
				{ID: "begin_date", Label: "begin_date", Kind: "string", Required: true, Shared: true},
				{ID: "end_date", Label: "end_date", Kind: "string", Required: true, Shared: true},
			},
		},
		{
			ID: "mini-visit-distribution", Group: "mini", Category: "数据分析", Label: "访问分布",
			Summary: "读取访问来源与访问时长分布，用于判断入口构成。",
			Method:  http.MethodPost, Path: "/datacube/getweanalysisappidvisitdistribution", JSON: true, Returns: "json",
			Params: []Param{
				{ID: "begin_date", Label: "begin_date", Kind: "string", Required: true, Shared: true},
				{ID: "end_date", Label: "end_date", Kind: "string", Required: true, Shared: true},
			},
		},
		{
			ID: "mini-user-portrait", Group: "mini", Category: "数据分析", Label: "用户画像",
			Summary: "读取访问用户的画像（地域、性别、年龄、终端）。",
			Method:  http.MethodPost, Path: "/datacube/getweanalysisappiduserportrait", JSON: true, Returns: "json",
			Params: []Param{
				{ID: "begin_date", Label: "begin_date", Kind: "string", Required: true, Shared: true},
				{ID: "end_date", Label: "end_date", Kind: "string", Required: true, Shared: true},
			},
		},
		{
			ID: "mini-weekly-retain", Group: "mini", Category: "数据分析", Label: "周留存",
			Summary: "读取新增用户的周留存数据。",
			Method:  http.MethodPost, Path: "/datacube/getweanalysisappidweeklyretaininfo", JSON: true, Returns: "json",
			Params: []Param{
				{ID: "begin_date", Label: "begin_date", Kind: "string", Required: true, Shared: true},
				{ID: "end_date", Label: "end_date", Kind: "string", Required: true, Shared: true},
			},
		},
		{
			ID: "mini-monthly-retain", Group: "mini", Category: "数据分析", Label: "月留存",
			Summary: "读取新增用户的月留存数据。",
			Method:  http.MethodPost, Path: "/datacube/getweanalysisappidmonthlyretaininfo", JSON: true, Returns: "json",
			Params: []Param{
				{ID: "begin_date", Label: "begin_date", Kind: "string", Required: true, Shared: true},
				{ID: "end_date", Label: "end_date", Kind: "string", Required: true, Shared: true},
			},
		},
		{
			ID: "mini-media-check", Group: "mini", Category: "内容安全", Label: "图片/音频异步检测",
			Summary: "提交图片或音频地址做内容安全检测，确认服务端的媒体拦截口径。",
			Method:  http.MethodPost, Path: "/wxa/media_check_async", JSON: true, Returns: "json",
			Note: "结果是异步的（返回 trace_id）；V2 同样要求 openid 在近两小时内访问过小程序。",
			Params: []Param{
				{ID: "media_url", Label: "media_url", Kind: "string", Required: true, Hint: "可公网访问的图片或音频地址"},
				{ID: "media_type", Label: "media_type", Kind: "number", Required: true, Default: "2", Hint: "1 音频 2 图片"},
				{ID: "version", Label: "version", Kind: "number", Default: "2"},
				{ID: "openid", Label: "openid", Kind: "string", Required: true, Shared: true},
				{ID: "scene", Label: "scene", Kind: "number", Default: "1", Shared: true},
			},
		},
		{
			ID: "mini-subscribe-templates", Group: "mini", Category: "订阅消息", Label: "订阅消息模板列表",
			Summary: "读取小程序已配置的订阅消息模板及其字段。",
			Method:  http.MethodGet, Path: "/wxaapi/newtmpl/gettemplate", Returns: "json",
		},
		// --- 微信公众号 ---
		{
			ID: "oa-followers", Group: "oa", Category: "用户", Label: "关注者 openid 列表",
			Summary: "读取公众号关注者 openid，用于确认凭据可触达的用户范围。",
			Method:  http.MethodGet, Path: "/cgi-bin/user/get", Returns: "json",
			Note:   "一次最多返回 10000 个 openid；用返回的 next_openid 继续翻页，total 是关注者总数。",
			Params: []Param{{ID: "next_openid", Label: "next_openid", Kind: "string", Hint: "留空从第一页开始", Shared: true}},
		},
		{
			ID: "oa-user-info", Group: "oa", Category: "用户", Label: "用户资料",
			Summary: "按 openid 读取单个用户资料。",
			Method:  http.MethodGet, Path: "/cgi-bin/user/info", Returns: "json",
			Params: []Param{
				{ID: "openid", Label: "openid", Kind: "string", Required: true, Shared: true},
				{ID: "lang", Label: "lang", Kind: "string", Default: "zh_CN", Shared: true},
			},
		},
		{
			ID: "oa-user-batchget", Group: "oa", Category: "用户", Label: "批量用户资料",
			Summary: "一次提交多个 openid，批量读取用户资料。",
			Method:  http.MethodPost, Path: "/cgi-bin/user/info/batchget", JSON: true, Returns: "json",
			// 这个接口的 body 是对象数组，不是扁平键值，所以按 JSON 参数收，
			// 由用户给出真实结构——拼错形态只会换回一个 errcode。
			Params: []Param{
				{ID: "user_list", Label: "user_list", Kind: "json", Required: true, Hint: `JSON 数组，如 [{"openid":"o1","lang":"zh_CN"}]，最多 100 个`},
			},
		},
		{
			ID: "oa-tags", Group: "oa", Category: "用户", Label: "标签列表",
			Summary: "读取公众号的用户标签及人数。",
			Method:  http.MethodGet, Path: "/cgi-bin/tags/get", Returns: "json",
		},
		{
			ID: "oa-tag-followers", Group: "oa", Category: "用户", Label: "标签下关注者",
			Summary: "读取某个标签下的 openid 列表。",
			Method:  http.MethodPost, Path: "/cgi-bin/user/tag/get", JSON: true, Returns: "json",
			Params: []Param{
				{ID: "tagid", Label: "tagid", Kind: "number", Required: true, Hint: "先用「标签列表」取", Shared: true},
				{ID: "next_openid", Label: "next_openid", Kind: "string", Shared: true},
			},
		},
		{
			ID: "oa-menu", Group: "oa", Category: "菜单与素材", Label: "自定义菜单",
			Summary: "读取公众号当前自定义菜单配置。",
			Method:  http.MethodGet, Path: "/cgi-bin/menu/get", Returns: "json",
		},
		{
			ID: "oa-material", Group: "oa", Category: "菜单与素材", Label: "素材列表",
			Summary: "分页读取公众号图文/图片等素材列表。",
			Method:  http.MethodPost, Path: "/cgi-bin/material/batchget_material", JSON: true, Returns: "json",
			Params: []Param{
				{ID: "type", Label: "type", Kind: "string", Required: true, Default: "news", Hint: "news | image | voice | video"},
				{ID: "offset", Label: "offset", Kind: "number", Default: "0", Shared: true},
				{ID: "count", Label: "count", Kind: "number", Default: "10", Hint: "1..20", Shared: true},
			},
		},
		{
			ID: "oa-material-count", Group: "oa", Category: "菜单与素材", Label: "素材总数",
			Summary: "读取各类素材的数量。",
			Method:  http.MethodGet, Path: "/cgi-bin/material/get_materialcount", Returns: "json",
		},
		{
			ID: "oa-callback-ip", Group: "oa", Category: "系统信息", Label: "微信服务器 IP",
			Summary: "读取微信回调与接口所用的服务器 IP 段。",
			Method:  http.MethodGet, Path: "/cgi-bin/getcallbackip", Returns: "json",
		},

		{
			ID: "oa-material-get", Group: "oa", Category: "菜单与素材", Label: "永久素材详情",
			Summary: "按 media_id 读取永久素材内容。",
			Method:  http.MethodPost, Path: "/cgi-bin/material/get_material", JSON: true, Returns: "json",
			Note:   "图文素材返回 JSON，图片/语音素材返回二进制，以最终响应为准。",
			Params: []Param{{ID: "media_id", Label: "media_id", Kind: "string", Required: true, Hint: "先用「素材列表」取"}},
		},
		{
			ID: "oa-qrcode-create", Group: "oa", Category: "菜单与素材", Label: "创建带参二维码",
			Summary: "创建带参数的公众号二维码，用于验证扫码事件与场景值处理。",
			Method:  http.MethodPost, Path: "/cgi-bin/qrcode/create", JSON: true, Returns: "json",
			Note: "返回 ticket，图片地址是 https://mp.weixin.qq.com/cgi-bin/showqrcode?ticket=<ticket>。",
			Params: []Param{
				{ID: "action_name", Label: "action_name", Kind: "string", Required: true, Hint: "QR_STR_SCENE 或 QR_LIMIT_STR_SCENE（永久）"},
				{ID: "scene_str", Label: "scene_str", Kind: "string", Required: true, Hint: "扫码时带出的场景值"},
				{ID: "expire_seconds", Label: "expire_seconds", Kind: "number", Hint: "临时码有效期，最长 2592000 秒"},
			},
		},
		{
			ID: "oa-selfmenu", Group: "oa", Category: "菜单与素材", Label: "自定义菜单配置",
			Summary: "读取公众号当前生效的菜单配置（含个性化菜单开关）。",
			Method:  http.MethodGet, Path: "/cgi-bin/get_current_selfmenu_info", Returns: "json",
		},
		{
			ID: "oa-tags-of-user", Group: "oa", Category: "用户", Label: "用户身上的标签",
			Summary: "读取某个 openid 被打上的标签。",
			Method:  http.MethodPost, Path: "/cgi-bin/tags/getidlist", JSON: true, Returns: "json",
			Params: []Param{{ID: "openid", Label: "openid", Kind: "string", Required: true, Shared: true}},
		},
		{
			ID: "oa-shorturl", Group: "oa", Category: "系统信息", Label: "长链接转短链",
			Summary: "把长链接转成短链，用于核对短链跳转与统计参数。",
			Method:  http.MethodPost, Path: "/cgi-bin/shorturl", JSON: true, Returns: "json",
			Params: []Param{
				{ID: "action", Label: "action", Kind: "string", Default: "long2short"},
				{ID: "long_url", Label: "long_url", Kind: "string", Required: true},
			},
		},
		{
			ID: "oa-quota-get", Group: "oa", Category: "系统信息", Label: "接口调用配额",
			Summary: "查询某个接口当天的调用上限与已用次数，用来看清这套凭据还能发多少请求。",
			Method:  http.MethodPost, Path: "/cgi-bin/openapi/quota/get", JSON: true, Returns: "json",
			Params: []Param{{ID: "cgi_path", Label: "cgi_path", Kind: "string", Required: true, Hint: "如 /cgi-bin/user/get"}},
		},
		{
			ID: "oa-rid-get", Group: "oa", Category: "系统信息", Label: "接口错误详情",
			Summary: "按 rid 读取某次调用的详细错误信息。",
			Method:  http.MethodPost, Path: "/cgi-bin/openapi/rid/get", JSON: true, Returns: "json",
			Params: []Param{{ID: "rid", Label: "rid", Kind: "string", Required: true, Hint: "错误响应里的 rid"}},
		},
		{
			ID: "oa-freepublish-batchget", Group: "oa", Category: "发布与草稿", Label: "已发布文章列表",
			Summary: "分页读取公众号已发布的文章。",
			Method:  http.MethodPost, Path: "/cgi-bin/freepublish/batchget", JSON: true, Returns: "json",
			Params: []Param{
				{ID: "offset", Label: "offset", Kind: "number", Default: "0", Shared: true},
				{ID: "count", Label: "count", Kind: "number", Default: "10", Hint: "1..20", Shared: true},
				{ID: "no_content", Label: "no_content", Kind: "number", Default: "1", Hint: "1 不返回正文", Shared: true},
			},
		},
		{
			ID: "oa-draft-batchget", Group: "oa", Category: "发布与草稿", Label: "草稿箱列表",
			Summary: "分页读取公众号草稿箱。",
			Method:  http.MethodPost, Path: "/cgi-bin/draft/batchget", JSON: true, Returns: "json",
			Params: []Param{
				{ID: "offset", Label: "offset", Kind: "number", Default: "0", Shared: true},
				{ID: "count", Label: "count", Kind: "number", Default: "10", Hint: "1..20", Shared: true},
				{ID: "no_content", Label: "no_content", Kind: "number", Default: "1", Shared: true},
			},
		},
		{
			ID: "oa-draft-count", Group: "oa", Category: "发布与草稿", Label: "草稿总数",
			Summary: "读取草稿箱中的图文数量。",
			Method:  http.MethodGet, Path: "/cgi-bin/draft/count", Returns: "json",
		},
		{
			ID: "oa-user-summary", Group: "oa", Category: "数据分析", Label: "用户增减数据",
			Summary: "读取每日新增与取消关注的用户数。",
			Method:  http.MethodPost, Path: "/datacube/getusersummary", JSON: true, Returns: "json",
			Params: []Param{
				{ID: "begin_date", Label: "begin_date", Kind: "string", Required: true, Shared: true},
				{ID: "end_date", Label: "end_date", Kind: "string", Required: true, Shared: true},
			},
		},
		{
			ID: "oa-user-cumulate", Group: "oa", Category: "数据分析", Label: "累计用户数据",
			Summary: "读取每日累计关注人数。",
			Method:  http.MethodPost, Path: "/datacube/getusercumulate", JSON: true, Returns: "json",
			Params: []Param{
				{ID: "begin_date", Label: "begin_date", Kind: "string", Required: true, Shared: true},
				{ID: "end_date", Label: "end_date", Kind: "string", Required: true, Shared: true},
			},
		},
		{
			ID: "oa-article-summary", Group: "oa", Category: "数据分析", Label: "图文群发每日数据",
			Summary: "读取群发图文的送达与阅读数据。",
			Method:  http.MethodPost, Path: "/datacube/getarticlesummary", JSON: true, Returns: "json",
			Params: []Param{
				{ID: "begin_date", Label: "begin_date", Kind: "string", Required: true, Shared: true},
				{ID: "end_date", Label: "end_date", Kind: "string", Required: true, Shared: true},
			},
		},
		{
			ID: "oa-article-total", Group: "oa", Category: "数据分析", Label: "图文群发总数据",
			Summary: "读取单篇图文从群发至今的累计数据。",
			Method:  http.MethodPost, Path: "/datacube/getarticletotal", JSON: true, Returns: "json",
			Params: []Param{
				{ID: "begin_date", Label: "begin_date", Kind: "string", Required: true, Shared: true},
				{ID: "end_date", Label: "end_date", Kind: "string", Required: true, Shared: true},
			},
		},
		// --- 企业微信 ---
		{
			ID: "work-user-list", Group: "work", Category: "通讯录", Label: "部门成员",
			Summary: "读取指定部门的成员列表，确认凭据可见的组织范围。",
			Method:  http.MethodGet, Path: "/cgi-bin/user/simplelist", Returns: "json",
			Note: "只返回该部门本身，不含子部门；要递归需先取「部门列表」，再逐层调用。",
			Params: []Param{
				{ID: "department_id", Label: "department_id", Kind: "string", Required: true, Default: "1", Shared: true},
			},
		},
		{
			ID: "work-user-list-detail", Group: "work", Category: "通讯录", Label: "部门成员详情",
			Summary: "读取部门成员的完整资料（手机号、邮箱等，取决于权限）。",
			Method:  http.MethodGet, Path: "/cgi-bin/user/list", Returns: "json",
			Params: []Param{
				{ID: "department_id", Label: "department_id", Kind: "string", Required: true, Shared: true},
				{ID: "fetch_child", Label: "fetch_child", Kind: "string", Default: "1", Hint: "1 递归子部门"},
			},
		},
		{
			ID: "work-user-get", Group: "work", Category: "通讯录", Label: "读取成员",
			Summary: "按 userid 读取单个成员的资料。",
			Method:  http.MethodGet, Path: "/cgi-bin/user/get", Returns: "json",
			Params: []Param{
				{ID: "userid", Label: "userid", Kind: "string", Required: true, Hint: "先用「部门成员」取", Shared: true},
			},
		},
		{
			ID: "work-department-list", Group: "work", Category: "通讯录", Label: "部门列表",
			Summary: "读取企业微信部门树。",
			Method:  http.MethodGet, Path: "/cgi-bin/department/list", Returns: "json",
			Params: []Param{{ID: "id", Label: "id", Kind: "string", Hint: "留空取全部部门", Shared: true}},
		},
		{
			ID: "work-department-get", Group: "work", Category: "通讯录", Label: "单个部门",
			Summary: "按部门 id 读取单个部门详情。",
			Method:  http.MethodGet, Path: "/cgi-bin/department/get", Returns: "json",
			Params: []Param{{ID: "id", Label: "id", Kind: "string", Required: true, Shared: true}},
		},
		{
			ID: "work-tag-list", Group: "work", Category: "通讯录", Label: "标签列表",
			Summary: "读取企业微信的标签列表。",
			Method:  http.MethodGet, Path: "/cgi-bin/tag/list", Returns: "json",
		},
		{
			ID: "work-agent-get", Group: "work", Category: "应用", Label: "读取应用",
			Summary: "按 agentid 读取自建应用的信息与可见范围。",
			Method:  http.MethodGet, Path: "/cgi-bin/agent/get", Returns: "json",
			Params: []Param{{ID: "agentid", Label: "agentid", Kind: "string", Required: true, Shared: true}},
		},
		{
			ID: "work-api-ip", Group: "work", Category: "系统信息", Label: "接口 IP 段",
			Summary: "读取企业微信接口的 IP 段。",
			Method:  http.MethodGet, Path: "/cgi-bin/get_api_domain_ip", Returns: "json",
		},
		{
			ID: "work-department-simplelist", Group: "work", Category: "通讯录", Label: "子部门列表",
			Summary: "读取某个部门下的直接子部门，用于逐层展开部门树。",
			Method:  http.MethodGet, Path: "/cgi-bin/department/simplelist", Returns: "json",
			Params: []Param{{ID: "id", Label: "id", Kind: "string", Hint: "留空取全部部门", Shared: true}},
		},
		{
			ID: "work-tag-members", Group: "work", Category: "通讯录", Label: "标签成员",
			Summary: "读取某个标签下的成员与部门。",
			Method:  http.MethodGet, Path: "/cgi-bin/tag/get", Returns: "json",
			Params: []Param{{ID: "tagid", Label: "tagid", Kind: "string", Required: true, Hint: "先用「标签列表」取", Shared: true}},
		},
	}
}

// sanitizeURLError strips *url.Error's full request URL from the error text:
// credentials ride in the request (secret in the token query), and a timeout
// would otherwise echo them back through the error message.
func sanitizeURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}
