package app

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhf883680/LayerWatch/internal/config"
)

// fakeLightCamera 模拟 HA 客户端 + 摄像头：按快照序号返回暗图或亮图，
// 用来验证「开灯后第几张图才变亮」的测量逻辑。
type fakeLightCamera struct {
	mu         sync.Mutex
	state      string
	calls      []string
	snapshots  int
	brightFrom int
	dark       []byte
	bright     []byte
}

func (f *fakeLightCamera) Check(context.Context) error { return nil }

func (f *fakeLightCamera) Snapshot(context.Context) ([]byte, error) {
	f.mu.Lock()
	f.snapshots++
	n := f.snapshots
	f.mu.Unlock()
	if f.brightFrom > 0 && n >= f.brightFrom {
		return f.bright, nil
	}
	return f.dark, nil
}

func (f *fakeLightCamera) EntityState(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, nil
}

func (f *fakeLightCamera) CallService(_ context.Context, domain, service string, _ any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, domain+"."+service)
	switch service {
	case "turn_on":
		f.state = "on"
	case "turn_off":
		f.state = "off"
	}
	return nil
}

func (f *fakeLightCamera) snapshotCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshots
}

func (f *fakeLightCamera) serviceCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeLightCamera) lightState() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func solidJPEG(t *testing.T, value uint8) []byte {
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

func newLightTestService(t *testing.T, camera *fakeLightCamera, delaySeconds int) (*Service, string) {
	t.Helper()
	dataDir := t.TempDir()
	cfgStore, err := config.Open(filepath.Join(dataDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.HomeAssistant.Token = "token"
	cfg.AI.Enabled = false
	cfg.Lighting = config.Lighting{Enabled: true, Entity: "light.test", DelaySeconds: delaySeconds}
	if err := cfgStore.Update(cfg); err != nil {
		t.Fatal(err)
	}
	return &Service{cfg: cfgStore, dataDir: dataDir, source: camera, entities: camera}, dataDir
}

func waitLightTest(t *testing.T, service *Service) LightTestResult {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		result := service.LightTestStatus()
		if result != nil && result.Status != LightTestRunning {
			return *result
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("照明测试没有在 5 秒内结束")
	return LightTestResult{}
}

func TestLightTestMeasuresDetectionLatency(t *testing.T) {
	// 基线 2 张之后：第 3 张（按设定延迟抓的第一张）仍是暗图，第 4 张才变亮。
	camera := &fakeLightCamera{state: "off", dark: solidJPEG(t, 20), bright: solidJPEG(t, 200), brightFrom: 4}
	service, _ := newLightTestService(t, camera, 0)

	started, err := service.StartLightTest(LightTestOptions{DelayMs: 0, IntervalMs: 100, TimeoutMs: 5000})
	if err != nil {
		t.Fatalf("StartLightTest err=%v", err)
	}
	if started.Status != LightTestRunning {
		t.Fatalf("启动后状态=%q，期望 running", started.Status)
	}

	result := waitLightTest(t, service)
	if result.Status != LightTestDone {
		t.Fatalf("状态=%q message=%q", result.Status, result.Message)
	}
	if result.DetectMs == nil {
		t.Fatalf("应当测到变亮画面，result=%+v", result)
	}
	if result.Attempts != 2 {
		t.Fatalf("开灯后抓图次数=%d，期望 2", result.Attempts)
	}
	if result.DelaySufficient == nil || *result.DelaySufficient {
		t.Fatalf("第一张按设定延迟抓到的图仍是暗图，DelaySufficient=%v", result.DelaySufficient)
	}
	if len(result.Frames) != 4 {
		t.Fatalf("总帧数=%d，期望 4（2 张基线 + 2 张开灯后）", len(result.Frames))
	}
	if !result.Frames[0].Baseline || !result.Frames[1].Baseline || result.Frames[2].Baseline {
		t.Fatalf("基线帧标记不对: %+v", result.Frames)
	}
	if result.Frames[3].Bright != true {
		t.Fatalf("最后一张应当判定为变亮: %+v", result.Frames[3])
	}
	if result.BaselineBrightness < 5 || result.BaselineBrightness > 12 {
		t.Fatalf("暗底亮度=%.1f，期望接近 20/255", result.BaselineBrightness)
	}
	if result.MaxBrightness < 70 {
		t.Fatalf("最高亮度=%.1f，期望接近 200/255", result.MaxBrightness)
	}
	if result.BrightImage == "" || result.BaselineImage == "" {
		t.Fatalf("应当给出暗底图和变亮图: %+v", result)
	}
	if !strings.Contains(result.Advice, "建议") {
		t.Fatalf("延迟不足时应给出建议，advice=%q", result.Advice)
	}
	if result.RestoredState != "off" || camera.lightState() != "off" {
		t.Fatalf("测试结束应关灯，restored=%q state=%q", result.RestoredState, camera.lightState())
	}
	if calls := camera.serviceCalls(); len(calls) != 2 || calls[0] != "light.turn_on" || calls[1] != "light.turn_off" {
		t.Fatalf("HA 调用=%v，期望 [light.turn_on light.turn_off]", calls)
	}
	if _, err := service.LightTestImagePath(result.ID, filepath.Base(result.BaselineImage)); err != nil {
		t.Fatalf("应当能取到测试截图: %v", err)
	}
}

func TestLightTestReportsSufficientDelay(t *testing.T) {
	// 基线 2 张之后，第 3 张（按设定延迟抓的第一张）就已经变亮。
	camera := &fakeLightCamera{state: "off", dark: solidJPEG(t, 20), bright: solidJPEG(t, 200), brightFrom: 3}
	service, _ := newLightTestService(t, camera, 1)

	if _, err := service.StartLightTest(LightTestOptions{DelayMs: 1000, IntervalMs: 100, TimeoutMs: 5000}); err != nil {
		t.Fatal(err)
	}
	result := waitLightTest(t, service)
	if result.Status != LightTestDone || result.DetectMs == nil {
		t.Fatalf("状态=%q detectMs=%v", result.Status, result.DetectMs)
	}
	if result.DelaySufficient == nil || !*result.DelaySufficient {
		t.Fatalf("第一张图已变亮，DelaySufficient=%v", result.DelaySufficient)
	}
	if result.DelayMs != 1000 {
		t.Fatalf("DelayMs=%d，期望 1000", result.DelayMs)
	}
	if result.Attempts != 1 {
		t.Fatalf("抓图次数=%d，期望 1", result.Attempts)
	}
	if !strings.Contains(result.Advice, "足够") {
		t.Fatalf("advice=%q", result.Advice)
	}
}

func TestLightTestRestoresLightThatWasOn(t *testing.T) {
	camera := &fakeLightCamera{state: "on", dark: solidJPEG(t, 20), bright: solidJPEG(t, 200), brightFrom: 3}
	service, _ := newLightTestService(t, camera, 0)

	if _, err := service.StartLightTest(LightTestOptions{DelayMs: 0, IntervalMs: 100, TimeoutMs: 5000}); err != nil {
		t.Fatal(err)
	}
	result := waitLightTest(t, service)
	if result.InitialState != "on" {
		t.Fatalf("InitialState=%q", result.InitialState)
	}
	if result.RestoredState != "on" || camera.lightState() != "on" {
		t.Fatalf("原本亮着的灯测试后应恢复为亮，restored=%q state=%q", result.RestoredState, camera.lightState())
	}
	calls := camera.serviceCalls()
	want := []string{"light.turn_off", "light.turn_on", "light.turn_on"}
	if len(calls) != len(want) {
		t.Fatalf("HA 调用=%v，期望 %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("HA 调用=%v，期望 %v", calls, want)
		}
	}
}

func TestLightTestTimesOutWithoutBrightFrame(t *testing.T) {
	camera := &fakeLightCamera{state: "off", dark: solidJPEG(t, 20), bright: solidJPEG(t, 200)}
	service, _ := newLightTestService(t, camera, 0)

	if _, err := service.StartLightTest(LightTestOptions{DelayMs: 0, IntervalMs: 200, TimeoutMs: 1000}); err != nil {
		t.Fatal(err)
	}
	result := waitLightTest(t, service)
	if result.Status != LightTestDone {
		t.Fatalf("状态=%q", result.Status)
	}
	if result.DetectMs != nil {
		t.Fatalf("不应测到变亮，detectMs=%v", *result.DetectMs)
	}
	if result.Attempts < 2 {
		t.Fatalf("超时前应抓多张图，attempts=%d", result.Attempts)
	}
	if !strings.Contains(result.Advice, "没有截到") {
		t.Fatalf("advice=%q", result.Advice)
	}
}

func TestLightTestRejectsConcurrentRuns(t *testing.T) {
	camera := &fakeLightCamera{state: "off", dark: solidJPEG(t, 20), bright: solidJPEG(t, 200)}
	service, _ := newLightTestService(t, camera, 0)

	if _, err := service.StartLightTest(LightTestOptions{DelayMs: 0, IntervalMs: 200, TimeoutMs: 2000}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.StartLightTest(LightTestOptions{DelayMs: 0, IntervalMs: 200, TimeoutMs: 2000}); err == nil {
		t.Fatal("并发启动应当被拒绝")
	}
	if !service.CancelLightTest() {
		t.Fatal("应当能取消进行中的测试")
	}
	result := waitLightTest(t, service)
	if result.Status != LightTestCanceled {
		t.Fatalf("取消后状态=%q", result.Status)
	}
}

func TestLightTestAdviceAndThreshold(t *testing.T) {
	if got := lightTestThreshold(0); got != lightTestMinDelta {
		t.Fatalf("暗底为 0 时门槛=%.1f，期望 %.1f", got, lightTestMinDelta)
	}
	if got := lightTestThreshold(2); got != 6 {
		t.Fatalf("暗底为 2 时门槛=%.1f，期望 6", got)
	}
	if got := lightTestThreshold(200); got != 215 {
		t.Fatalf("很亮的画面门槛应封顶，got=%.1f", got)
	}
	detect := int64(2400)
	sufficient := false
	advice := lightTestAdvice(LightTestResult{DetectMs: &detect, DelayMs: 300, TimeoutMs: 20000, DelaySufficient: &sufficient})
	if !strings.Contains(advice, "300 ms 不够") || !strings.Contains(advice, "3200 ms") {
		t.Fatalf("advice=%q", advice)
	}
}

func TestLightTestImagePathValidation(t *testing.T) {
	camera := &fakeLightCamera{state: "off", dark: solidJPEG(t, 20), bright: solidJPEG(t, 200), brightFrom: 3}
	service, _ := newLightTestService(t, camera, 0)
	if _, err := service.StartLightTest(LightTestOptions{DelayMs: 0, IntervalMs: 100, TimeoutMs: 5000}); err != nil {
		t.Fatal(err)
	}
	result := waitLightTest(t, service)

	path, err := service.LightTestImagePath(result.ID, "frame-001.jpg")
	if err != nil {
		t.Fatalf("frame-001.jpg 应当可读: %v", err)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("截图文件应当存在: %v", statErr)
	}

	for _, tc := range []struct{ run, name string }{
		{result.ID, "../config.yaml"},
		{result.ID, "frame-001.png"},
		{"../../etc", "frame-001.jpg"},
		{"20260101-000000.000", "frame-001.jpg"},
	} {
		if _, err := service.LightTestImagePath(tc.run, tc.name); err == nil {
			t.Fatalf("run=%q name=%q 应当被拒绝", tc.run, tc.name)
		}
	}
}

func TestImageBrightnessAndThumbnail(t *testing.T) {
	dark := solidJPEG(t, 0)
	bright := solidJPEG(t, 255)
	darkValue, err := imageBrightness(dark)
	if err != nil {
		t.Fatal(err)
	}
	brightValue, err := imageBrightness(bright)
	if err != nil {
		t.Fatal(err)
	}
	if darkValue > 1 || brightValue < 99 {
		t.Fatalf("亮度计算异常: dark=%.1f bright=%.1f", darkValue, brightValue)
	}
	if _, err := imageBrightness([]byte("not an image")); err == nil {
		t.Fatal("非图片数据应当报错")
	}

	big := image.NewRGBA(image.Rect(0, 0, 1280, 720))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, big, nil); err != nil {
		t.Fatal(err)
	}
	thumb, err := thumbnailJPEG(buf.Bytes(), 640, 70)
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(bytes.NewReader(thumb))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != 640 || img.Bounds().Dy() != 360 {
		t.Fatalf("缩略图尺寸=%v，期望 640x360", img.Bounds())
	}
}
