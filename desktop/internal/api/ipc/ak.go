package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/langbyyi/wxtap/desktop/internal/ak"
)

// VerifyAK implements ak.verify: an explicit credential check against the
// official WeChat / WeCom token endpoint. Invalid credentials are returned as
// a normal result so the UI can distinguish them from transport failures.
func VerifyAK(ctx context.Context, params json.RawMessage) (any, error) {
	var request ak.Request
	if err := json.Unmarshal(params, &request); err != nil {
		return nil, fmt.Errorf("请求参数必须是 JSON 对象: %w", err)
	}
	request.Mode = strings.TrimSpace(strings.ToLower(request.Mode))
	request.AccessKey = strings.TrimSpace(request.AccessKey)
	request.SecretKey = strings.TrimSpace(request.SecretKey)
	request.FakeIP = strings.TrimSpace(request.FakeIP)
	if request.Mode == "" || request.AccessKey == "" || request.SecretKey == "" {
		return nil, fmt.Errorf("mode、access_key 和 secret_key 不能为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	verifyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return ak.Default.Verify(verifyCtx, request)
}
