package replay

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// 合法 JSON 往返解析：log.version/creator/entries 骨架、请求方法与 URL、
// postData 的 mimeType 取请求侧 Content-Type。
func TestBuildHARRoundTrips(t *testing.T) {
	har := BuildHAR([]HAREntry{{
		When: time.Date(2026, 9, 25, 8, 30, 5, 250_000_000, time.UTC),
		Target: Target{
			URL:    "https://api.example.com/v2/user?uid=1001&src=wx",
			Method: "POST",
			Headers: map[string]string{
				"Content-Type":  "application/json",
				"Authorization": "Bearer tok",
			},
			Body: `{"uid":1001}`,
		},
		Status:      200,
		RespHeaders: map[string]string{"Content-Type": "application/json; charset=utf-8"},
		Body:        []byte(`{"ok":true}`),
	}})

	var doc struct {
		Log struct {
			Version string `json:"version"`
			Creator struct {
				Name string `json:"name"`
			} `json:"creator"`
			Entries []struct {
				StartedDateTime string `json:"startedDateTime"`
				Request         struct {
					Method      string `json:"method"`
					URL         string `json:"url"`
					QueryString []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"queryString"`
					PostData *struct {
						MimeType string `json:"mimeType"`
						Text     string `json:"text"`
					} `json:"postData"`
				} `json:"request"`
				Response struct {
					Status  int `json:"status"`
					Content struct {
						Size     int    `json:"size"`
						MimeType string `json:"mimeType"`
						Text     string `json:"text"`
					} `json:"content"`
					HeadersSize int `json:"headersSize"`
				} `json:"response"`
				Timings map[string]float64 `json:"timings"`
			} `json:"entries"`
		} `json:"log"`
	}
	if err := json.Unmarshal(har, &doc); err != nil {
		t.Fatalf("输出必须是合法 JSON: %v", err)
	}
	if doc.Log.Version != "1.2" || doc.Log.Creator.Name != "WxTap" {
		t.Fatalf("log 骨架不对: %+v", doc.Log)
	}
	if len(doc.Log.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(doc.Log.Entries))
	}
	entry := doc.Log.Entries[0]
	if entry.Request.Method != "POST" || entry.Request.URL != "https://api.example.com/v2/user?uid=1001&src=wx" {
		t.Fatalf("请求行不对: %+v", entry.Request)
	}
	if entry.StartedDateTime != "2026-09-25T08:30:05.250Z" {
		t.Fatalf("startedDateTime = %q", entry.StartedDateTime)
	}
	if entry.Request.PostData == nil || entry.Request.PostData.MimeType != "application/json" || entry.Request.PostData.Text != `{"uid":1001}` {
		t.Fatalf("postData 不对: %+v", entry.Request.PostData)
	}
	if entry.Response.Status != 200 || entry.Response.Content.Text != `{"ok":true}` || entry.Response.Content.Size != 11 {
		t.Fatalf("响应侧不对: %+v", entry.Response)
	}
	if entry.Response.HeadersSize != -1 {
		t.Fatalf("headersSize 占位必须是 -1: %d", entry.Response.HeadersSize)
	}
	if entry.Timings["send"] != 0 || entry.Timings["wait"] != 0 || entry.Timings["receive"] != 0 {
		t.Fatalf("timings 必须全 0 占位: %+v", entry.Timings)
	}
}

// queryString 从 URL 解析而来：多值、顺序、空参都照实拆。
func TestBuildHARParsesQueryString(t *testing.T) {
	har := BuildHAR([]HAREntry{{
		Target: Target{URL: "https://api.example.com/list?b=2&a=1&a=0", Method: "GET"},
	}})
	var doc struct {
		Log struct {
			Entries []struct {
				Request struct {
					QueryString []struct {
						Name  string `json:"name"`
						Value string `json:"value"`
					} `json:"queryString"`
				} `json:"request"`
			} `json:"entries"`
		} `json:"log"`
	}
	if err := json.Unmarshal(har, &doc); err != nil {
		t.Fatal(err)
	}
	pairs := doc.Log.Entries[0].Request.QueryString
	if len(pairs) != 3 {
		t.Fatalf("queryString = %+v, want 3 对", pairs)
	}
	// 名字排序，多值按原序展开。
	want := []struct{ name, value string }{{"a", "1"}, {"a", "0"}, {"b", "2"}}
	for i, expect := range want {
		if pairs[i].Name != expect.name || pairs[i].Value != expect.value {
			t.Fatalf("queryString[%d] = %+v, want %s=%s", i, pairs[i], expect.name, expect.value)
		}
	}
}

// UTF-8 正文走 text 原文、不带 encoding；非 UTF-8 正文走 base64 + encoding，
// 解码后必须逐字节还原。
func TestBuildHAREncodesNonUTF8BodiesAsBase64(t *testing.T) {
	har := BuildHAR([]HAREntry{
		{Target: Target{URL: "https://api.example.com/text", Method: "GET"}, Body: []byte("你好，世界")},
		{Target: Target{URL: "https://api.example.com/bin", Method: "GET"}, Body: []byte{0xff, 0xfe, 0x00, 0x01}},
	})
	var doc struct {
		Log struct {
			Entries []struct {
				Response struct {
					Content struct {
						Text     string `json:"text"`
						Encoding string `json:"encoding"`
					} `json:"content"`
				} `json:"response"`
			} `json:"entries"`
		} `json:"log"`
	}
	if err := json.Unmarshal(har, &doc); err != nil {
		t.Fatal(err)
	}
	utf8Entry := doc.Log.Entries[0].Response.Content
	if utf8Entry.Text != "你好，世界" || utf8Entry.Encoding != "" {
		t.Fatalf("UTF-8 正文必须原文进 text: %+v", utf8Entry)
	}
	binEntry := doc.Log.Entries[1].Response.Content
	if binEntry.Encoding != "base64" {
		t.Fatalf("非 UTF-8 正文必须标 base64: %+v", binEntry)
	}
	decoded, err := base64.StdEncoding.DecodeString(binEntry.Text)
	if err != nil || string(decoded) != string([]byte{0xff, 0xfe, 0x00, 0x01}) {
		t.Fatalf("base64 解码必须逐字节还原: %q err=%v", binEntry.Text, err)
	}
}

// 空入口、无 body、无 Content-Type 都走安全路径：entries 是 [] 不是 null，
// 缺省 mimeType 落 application/octet-stream。
func TestBuildHARHandlesEmptyInputs(t *testing.T) {
	har := BuildHAR(nil)
	if string(har) == "" {
		t.Fatal("零入口也应产出可解析的文档")
	}
	var doc struct {
		Log struct {
			Entries []any `json:"entries"`
		} `json:"log"`
	}
	if err := json.Unmarshal(har, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Log.Entries == nil || len(doc.Log.Entries) != 0 {
		t.Fatalf("entries 必须是 []: %s", har)
	}

	har = BuildHAR([]HAREntry{{Target: Target{URL: "https://api.example.com/x", Method: "GET"}}})
	var one struct {
		Log struct {
			Entries []struct {
				Request struct {
					PostData *struct {
						MimeType string `json:"mimeType"`
					} `json:"postData"`
				} `json:"request"`
				Response struct {
					Content struct {
						MimeType string `json:"mimeType"`
					} `json:"content"`
				} `json:"response"`
			} `json:"entries"`
		} `json:"log"`
	}
	if err := json.Unmarshal(har, &one); err != nil {
		t.Fatal(err)
	}
	entry := one.Log.Entries[0]
	if entry.Request.PostData != nil {
		t.Fatalf("无 body 不得造 postData: %+v", entry.Request.PostData)
	}
	if entry.Response.Content.MimeType != harOctetStream {
		t.Fatalf("缺 Content-Type 的缺省 mimeType 不对: %+v", entry.Response.Content)
	}
}
