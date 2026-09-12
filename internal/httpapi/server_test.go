package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhf883680/LayerWatch/internal/app"
	"github.com/zhf883680/LayerWatch/internal/config"
	"github.com/zhf883680/LayerWatch/internal/store"
	"github.com/zhf883680/LayerWatch/internal/vision"
)

func TestVideoContentType(t *testing.T) {
	tests := map[string]string{
		"print-1-20260910.mp4": "video/mp4",
		"PRINT.MP4":            "video/mp4",
		"clip.m4v":             "video/mp4",
		"clip.mov":             "video/quicktime",
		"clip.webm":            "video/webm",
		"clip.mkv":             "video/x-matroska",
		"notes.txt":            "application/octet-stream",
	}
	for path, want := range tests {
		if got := videoContentType(path); got != want {
			t.Fatalf("videoContentType(%q)=%q want %q", path, got, want)
		}
	}
}

// lightCamera 是照明测试用的假 HA 客户端：按快照序号返回暗图或亮图。
type lightCamera struct {
	mu         sync.Mutex
	state      string
	snapshots  int
	brightFrom int
	dark       []byte
	bright     []byte
}

func (c *lightCamera) Check(context.Context) error { return nil }

func (c *lightCamera) Snapshot(context.Context) ([]byte, error) {
	c.mu.Lock()
	c.snapshots++
	n := c.snapshots
	c.mu.Unlock()
	if c.brightFrom > 0 && n >= c.brightFrom {
		return c.bright, nil
	}
	return c.dark, nil
}

func (c *lightCamera) EntityState(context.Context, string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state, nil
}

func (c *lightCamera) CallService(_ context.Context, _, service string, _ any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if service == "turn_on" {
		c.state = "on"
	} else if service == "turn_off" {
		c.state = "off"
	}
	return nil
}

func (c *lightCamera) Notify(context.Context, string, string) error { return nil }

type stubDetector struct{}

func (stubDetector) Enabled() bool { return false }

func (stubDetector) Analyze(context.Context, [][]byte) (*vision.Result, error) { return nil, nil }

type stubEncoder struct{}

func (stubEncoder) Binary() string { return "ffmpeg" }

func (stubEncoder) Encode(context.Context, string, string) error { return nil }

func grayJPEG(t *testing.T, value uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: value, G: value, B: value, A: 255})
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// TestLightTestRoutes 覆盖照明测试页和接口：启动 → 轮询 → 取回截图。
func TestLightTestRoutes(t *testing.T) {
	dataDir := t.TempDir()
	cfgStore, err := config.Open(filepath.Join(dataDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.HomeAssistant.Token = "token"
	cfg.AI.Enabled = false
	cfg.Lighting = config.Lighting{Enabled: true, Entity: "light.test", DelaySeconds: 1}
	if err := cfgStore.Update(cfg); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dataDir, "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	camera := &lightCamera{state: "off", dark: grayJPEG(t, 20), bright: grayJPEG(t, 200), brightFrom: 3}
	application := app.New(cfgStore, db, dataDir, camera, camera, camera, stubDetector{}, stubEncoder{})
	handler := New(cfgStore, application).Handler()

	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/light-test", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("GET /light-test 状态码=%d", page.Code)
	}
	if body := page.Body.String(); !strings.Contains(body, "/api/test/light-capture") || !strings.Contains(body, "点亮灯并截图") {
		t.Fatalf("测试页内容不完整: %d 字节", len(body))
	}

	start := httptest.NewRecorder()
	handler.ServeHTTP(start, httptest.NewRequest(http.MethodPost, "/api/test/light-capture?delayMs=200&intervalMs=100&timeoutMs=5000", nil))
	if start.Code != http.StatusAccepted {
		t.Fatalf("启动测试状态码=%d body=%s", start.Code, start.Body.String())
	}

	var result app.LightTestResult
	deadline := time.Now().Add(5 * time.Second)
	for {
		status := httptest.NewRecorder()
		handler.ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/test/light-capture", nil))
		if status.Code != http.StatusOK {
			t.Fatalf("查询状态码=%d", status.Code)
		}
		if err := json.Unmarshal(status.Body.Bytes(), &result); err != nil {
			t.Fatalf("解析状态失败: %v", err)
		}
		if result.Status != app.LightTestRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("照明测试未在 5 秒内结束")
		}
		time.Sleep(30 * time.Millisecond)
	}
	if result.Status != app.LightTestDone || result.DetectMs == nil {
		t.Fatalf("结果异常: status=%q detectMs=%v message=%q", result.Status, result.DetectMs, result.Message)
	}
	if result.BrightImage == "" {
		t.Fatal("结果里没有变亮截图地址")
	}

	imageResp := httptest.NewRecorder()
	handler.ServeHTTP(imageResp, httptest.NewRequest(http.MethodGet, result.BrightImage, nil))
	if imageResp.Code != http.StatusOK {
		t.Fatalf("取截图状态码=%d", imageResp.Code)
	}
	if ct := imageResp.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("截图 Content-Type=%q", ct)
	}
	if imageResp.Body.Len() == 0 {
		t.Fatal("截图内容为空")
	}

	// 目录穿越、越界文件名、不存在的测试都要被挡掉。
	// 未转义的 ".." 会被 net/http 的 mux 先重定向（307），转义后的则落到 404。
	for _, path := range []string{
		"/api/test/light-capture/image/" + result.ID + "/%2e%2e%2fconfig.yaml",
		"/api/test/light-capture/image/" + result.ID + "/frame-999.jpg",
		"/api/test/light-capture/image/20260101-000000.000/frame-001.jpg",
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("非法截图请求 %s 状态码=%d，期望 404", path, rec.Code)
		}
	}

	// 取消：没有进行中的测试时返回 404。
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/test/light-capture", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("取消空闲测试状态码=%d，期望 404", rec.Code)
	}
}
