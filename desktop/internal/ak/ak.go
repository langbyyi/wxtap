// Package ak verifies leaked WeChat credentials against their official token
// endpoints. The surface is intentionally small: it proves whether a supplied
// AppID/CorpID + Secret pair can obtain an access token. It never enumerates
// users, calls business APIs, or performs writes.
package ak

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Body limits follow the ak_verify contract: truncated evidence by default,
// with a hard cap so a hostile endpoint cannot force a large allocation.
const (
	defaultBodyLimit = 2000
	maxBodyLimit     = 8000
	defaultTimeout   = 12 * time.Second
)

// Request is one explicit credential verification. Mode is oa (公众号),
// mini (小程序), or work (企业微信).
type Request struct {
	Mode         string `json:"mode"`
	AccessKey    string `json:"access_key"`
	SecretKey    string `json:"secret_key"`
	FakeIP       string `json:"fake_ip,omitempty"`
	IncludeToken bool   `json:"include_token,omitempty"`
	BodyLimit    int    `json:"body_limit,omitempty"`
}

// Result always contains the caller-visible verdict. Token is empty unless the
// caller explicitly opts into the sensitive value; the fingerprint is enough
// to correlate runs without exposing the credential.
type Result struct {
	Mode     string `json:"mode"`
	Valid    bool   `json:"valid"`
	Endpoint string `json:"endpoint"`
	// RequestURL is the exact request that went out, credential included: the
	// UI shows the exchange as a packet, and a masked packet is not the packet.
	RequestURL       string `json:"request_url,omitempty"`
	HTTPStatus       int    `json:"http_status"`
	ErrCode          int    `json:"errcode"`
	ErrMsg           string `json:"errmsg"`
	ExpiresIn        int    `json:"expires_in,omitempty"`
	Token            string `json:"token,omitempty"`
	TokenFingerprint string `json:"token_fingerprint,omitempty"`
	Body             any    `json:"body,omitempty"`
}

// Verifier calls one WeChat-compatible token endpoint. Base exists for tests;
// production always uses the default zero value.
type Verifier struct {
	Base string
	HTTP *http.Client
}

// Default is the production verifier used by the IPC handler.
var Default = &Verifier{HTTP: &http.Client{Timeout: defaultTimeout}}

// Verify performs one read-only official token request. Invalid credentials
// are a normal result (valid=false); only transport or request construction
// failures are Go errors.
func (v *Verifier) Verify(ctx context.Context, request Request) (Result, error) {
	mode := strings.TrimSpace(strings.ToLower(request.Mode))
	accessKey := strings.TrimSpace(request.AccessKey)
	secretKey := strings.TrimSpace(request.SecretKey)
	if accessKey == "" || secretKey == "" {
		return Result{Mode: mode}, fmt.Errorf("access_key 和 secret_key 不能为空")
	}

	endpoint, keyName, secretName := endpointFor(v.Base, mode)
	if endpoint == "" {
		return Result{Mode: mode}, fmt.Errorf("mode 必须是 oa / mini / work")
	}

	query := url.Values{}
	query.Set("grant_type", "client_credential")
	query.Set(keyName, accessKey)
	query.Set(secretName, secretKey)
	requestURL := endpoint + "?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return Result{Mode: mode}, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if fakeIP := strings.TrimSpace(request.FakeIP); fakeIP != "" {
		if err := validateIPs(fakeIP); err != nil {
			return Result{Mode: mode}, err
		}
		req.Header.Set("X-Forwarded-For", fakeIP)
		req.Header.Set("X-Real-IP", firstIP(fakeIP))
	}

	response, err := v.client().Do(req)
	if err != nil {
		return Result{Mode: mode}, fmt.Errorf("请求失败: %w", sanitizeURLError(err))
	}
	defer func() { _ = response.Body.Close() }()

	limit := request.BodyLimit
	if limit <= 0 {
		limit = defaultBodyLimit
	}
	if limit > maxBodyLimit {
		limit = maxBodyLimit
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(limit)+1))
	if err != nil {
		return Result{Mode: mode}, fmt.Errorf("读取响应失败: %w", err)
	}
	if len(body) > limit {
		return Result{Mode: mode}, fmt.Errorf("响应超过 %d 字节", limit)
	}

	result := Result{
		Mode:       mode,
		Endpoint:   endpoint,
		RequestURL: requestURL,
		HTTPStatus: response.StatusCode,
		Body:       decodeBody(body),
	}
	var payload struct {
		ErrCode   *int   `json:"errcode"`
		ErrMsg    string `json:"errmsg"`
		Token     string `json:"access_token"`
		ExpiresIn int    `json:"expires_in"`
	}
	_ = json.Unmarshal(body, &payload)
	if payload.ErrCode != nil {
		result.ErrCode = *payload.ErrCode
	}
	result.ErrMsg = payload.ErrMsg
	result.ExpiresIn = payload.ExpiresIn
	if payload.Token != "" {
		result.TokenFingerprint = fingerprint(payload.Token)
		if request.IncludeToken {
			result.Token = payload.Token
		}
	}
	result.Valid = payload.Token != "" && (payload.ErrCode == nil || *payload.ErrCode == 0)
	return result, nil
}

func (v *Verifier) client() *http.Client {
	if v.HTTP != nil {
		return v.HTTP
	}
	return &http.Client{Timeout: defaultTimeout}
}

func endpointFor(base, mode string) (endpoint, keyName, secretName string) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	switch mode {
	case "oa", "mini":
		if base == "" {
			base = "https://api.weixin.qq.com"
		}
		return base + "/cgi-bin/token", "appid", "secret"
	case "work":
		if base == "" {
			base = "https://qyapi.weixin.qq.com"
		}
		return base + "/cgi-bin/gettoken", "corpid", "corpsecret"
	default:
		return "", "", ""
	}
}

func decodeBody(body []byte) any {
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return string(body)
	}
	return decoded
}

func validateIPs(value string) error {
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" || net.ParseIP(part) == nil {
			return fmt.Errorf("fake_ip 必须是有效 IP")
		}
	}
	return nil
}

func firstIP(value string) string {
	part, _, _ := strings.Cut(value, ",")
	return strings.TrimSpace(part)
}

func fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:16]
}

// sanitizeURLError strips *url.Error's full request URL from the error text:
// the query carries the appid/secret in plaintext, and credentials must not
// leak out through the error channel (timeouts and DNS failures would
// otherwise echo the whole signed URL back to the caller).
func sanitizeURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}
