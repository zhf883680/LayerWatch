package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zhf883680/LayerWatch/internal/config"
)

func TestParseResult(t *testing.T) {
	result, err := parseResult("```json\n{\"status\":\"spaghetti\",\"confidence\":0.91,\"reason\":\"丝料成团\"}\n```")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusSpaghetti || result.Confidence != 0.91 || !result.Abnormal() {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestParseResultUnknownStatus(t *testing.T) {
	result, err := parseResult(`{"status":"not-a-status","confidence":2,"reason":"x"}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusUnknown || result.Confidence != 1 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func testConfigStore(t *testing.T, mutate func(cfg *config.Config)) *config.Store {
	t.Helper()
	store, err := config.Open(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.AI.Enabled = true
	cfg.AI.APIKey = "test-key"
	cfg.AI.Model = "qwen3-vl-flash"
	if mutate != nil {
		mutate(&cfg)
	}
	if err := store.Update(cfg); err != nil {
		t.Fatal(err)
	}
	return store
}

func tinyJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// 测试不走真实网络：用一个假 Transport 直接接住请求，沙箱里也能跑。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(status int, payload any) *http.Response {
	body, _ := json.Marshal(payload)
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}

func chatOK(content string, usage map[string]any) map[string]any {
	out := map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
	}
	if usage != nil {
		out["usage"] = usage
	}
	return out
}

// 缩图要真正作用到请求体里（省 token 的关键路径），detail 固定为 auto。
func TestAnalyzeAppliesCompressionAndDetail(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	service := New(testConfigStore(t, func(cfg *config.Config) {
		cfg.AI.BaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
		cfg.AI.MaxImageWidth = 320
	}))
	service.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode body: %v", err)
		}
		return jsonResponse(http.StatusOK, chatOK(
			`{"status":"normal","confidence":0.9,"reason":"ok"}`,
			map[string]any{"prompt_tokens": 1736, "completion_tokens": 56, "total_tokens": 1792,
				"prompt_tokens_details": map[string]any{"cached_tokens": 512}},
		)), nil
	})}

	result, err := service.Analyze(context.Background(), [][]byte{tinyJPEG(t, 800, 400)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusNormal {
		t.Fatalf("status=%q", result.Status)
	}
	if gotAuth != "Bearer test-key" {
		t.Fatalf("authorization=%q", gotAuth)
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%d", len(msgs))
	}
	user, _ := msgs[1].(map[string]any)
	parts, _ := user["content"].([]any)
	imgPart, _ := parts[len(parts)-1].(map[string]any)
	url, _ := imgPart["image_url"].(map[string]any)
	if detail, _ := url["detail"].(string); detail != "auto" {
		t.Fatalf("detail=%q", detail)
	}
	dataURLText, _ := url["url"].(string)
	raw := strings.TrimPrefix(dataURLText, "data:image/jpeg;base64,")
	if raw == dataURLText {
		t.Fatalf("unexpected image url: %.40s", dataURLText)
	}
	scaled, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(bytes.NewReader(scaled))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 320 {
		t.Fatalf("发送前应缩到 320 宽，实际 %d", img.Bounds().Dx())
	}
}

// maxImageWidth=0 表示不压缩，原图直发。
func TestAnalyzeKeepsOriginalWhenCompressionDisabled(t *testing.T) {
	original := tinyJPEG(t, 320, 160)
	var gotBody map[string]any
	service := New(testConfigStore(t, func(cfg *config.Config) {
		cfg.AI.BaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
		cfg.AI.MaxImageWidth = 0
	}))
	service.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		return jsonResponse(http.StatusOK, chatOK(`{"status":"normal","confidence":0.5,"reason":"ok"}`, nil)), nil
	})}
	if _, err := service.Analyze(context.Background(), [][]byte{original}); err != nil {
		t.Fatal(err)
	}
	msgs, _ := gotBody["messages"].([]any)
	user, _ := msgs[1].(map[string]any)
	parts, _ := user["content"].([]any)
	imgPart, _ := parts[len(parts)-1].(map[string]any)
	url, _ := imgPart["image_url"].(map[string]any)
	dataURLText, _ := url["url"].(string)
	if dataURLText != dataURL(original) {
		t.Fatal("关闭压缩时应原样发送")
	}
}

// imageSource=temp 时先上传到百炼临时 OSS，并把 oss:// URL 发给模型；同一张图只上传一次。
func TestAnalyzeUsesTempImageSourceWithCache(t *testing.T) {
	var uploads int32
	var gotBody map[string]any
	var gotOssHeader string
	service := New(testConfigStore(t, func(cfg *config.Config) {
		cfg.AI.BaseURL = "https://dashscope.aliyuncs.com/compatible-mode/v1"
		cfg.AI.ImageSource = config.ImageSourceTemp
		cfg.AI.MaxImageWidth = 0
	}))
	service.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/uploads"):
			return jsonResponse(http.StatusOK, map[string]any{"data": map[string]any{
				"upload_dir": "dashscope-temp/1", "upload_host": "https://oss.example.com",
				"policy": "p", "signature": "s", "oss_access_key_id": "ak",
			}}), nil
		case r.URL.Host == "oss.example.com":
			atomic.AddInt32(&uploads, 1)
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
		default:
			gotOssHeader = r.Header.Get("X-DashScope-OssResourceResolve")
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			return jsonResponse(http.StatusOK, chatOK(`{"status":"normal","confidence":0.7,"reason":"ok"}`, nil)), nil
		}
	})}
	img := tinyJPEG(t, 64, 32)
	for i := 0; i < 2; i++ {
		if _, err := service.Analyze(context.Background(), [][]byte{img}); err != nil {
			t.Fatal(err)
		}
	}
	if got := atomic.LoadInt32(&uploads); got != 1 {
		t.Fatalf("同一张图应只上传一次，实际 %d 次", got)
	}
	if gotOssHeader != "enable" {
		t.Fatalf("使用 oss:// 时必须带 X-DashScope-OssResourceResolve，实际 %q", gotOssHeader)
	}
	msgs, _ := gotBody["messages"].([]any)
	user, _ := msgs[1].(map[string]any)
	parts, _ := user["content"].([]any)
	imgPart, _ := parts[len(parts)-1].(map[string]any)
	url, _ := imgPart["image_url"].(map[string]any)
	if u, _ := url["url"].(string); !strings.HasPrefix(u, "oss://") {
		t.Fatalf("image url 应为 oss:// 开头，实际 %.40s", u)
	}
}
