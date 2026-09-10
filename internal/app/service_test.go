package app

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zhf883680/LayerWatch/internal/config"
	"github.com/zhf883680/LayerWatch/internal/store"
	"github.com/zhf883680/LayerWatch/internal/vision"
)

type fakeSource struct {
	calls int
	data  []byte
}

func (f *fakeSource) Check(context.Context) error { return nil }

func (f *fakeSource) Snapshot(context.Context) ([]byte, error) {
	f.calls++
	return f.data, nil
}

type fakeEntities struct {
	states map[string]string
}

func (f fakeEntities) EntityState(_ context.Context, entityID string) (string, error) {
	return f.states[entityID], nil
}

type fakeNotifier struct{}

func (fakeNotifier) Notify(context.Context, string, string) error { return nil }

type countingDetector struct {
	mu    sync.Mutex
	calls int
}

func (d *countingDetector) Enabled() bool { return true }
func (d *countingDetector) Analyze(context.Context, [][]byte) (*vision.Result, error) {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	return &vision.Result{Status: vision.StatusNormal, Confidence: 0.95}, nil
}
func (d *countingDetector) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

type fakeDetector struct{}

func (fakeDetector) Enabled() bool { return false }
func (fakeDetector) Analyze(context.Context, [][]byte) (*vision.Result, error) {
	return nil, nil
}

type fakeEncoder struct{}

func (fakeEncoder) Binary() string { return "fake-ffmpeg" }
func (fakeEncoder) Encode(_ context.Context, framesDir, outputPath string) error {
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(framesDir)
	if err != nil {
		return err
	}
	if len(entries) != 2 {
		return os.ErrInvalid
	}
	return os.WriteFile(outputPath, []byte("video"), 0o644)
}

func TestLayerCaptureAndEncode(t *testing.T) {
	dataDir := t.TempDir()
	cfgStore, err := config.Open(filepath.Join(dataDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.HomeAssistant.Token = "token"
	cfg.AI.APIKey = "key"
	cfg.AI.Enabled = false
	cfg.Trigger.Mode = config.TriggerLayer
	if err := cfgStore.Update(cfg); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dataDir, "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source := &fakeSource{data: testJPEG(t)}
	service := New(cfgStore, db, dataDir, source, fakeEntities{}, fakeNotifier{}, fakeDetector{}, fakeEncoder{})
	if _, _, err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, captured, err := service.CaptureLayer(context.Background(), 1); err != nil || !captured {
		t.Fatalf("layer 1 captured=%v err=%v", captured, err)
	}
	if _, captured, err := service.CaptureLayer(context.Background(), 1); err != nil || captured {
		t.Fatalf("duplicate layer captured=%v err=%v", captured, err)
	}
	if _, captured, err := service.CaptureLayer(context.Background(), 2); err != nil || !captured {
		t.Fatalf("layer 2 captured=%v err=%v", captured, err)
	}
	session, found, err := service.Stop(context.Background())
	if err != nil || !found {
		t.Fatalf("stop found=%v err=%v", found, err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current, err := db.GetSession(session.ID)
		if err == nil && current.Status == store.SessionComplete {
			videos, err := db.ListVideos(10)
			if err != nil || len(videos) != 1 || videos[0].FrameCount != 2 {
				t.Fatalf("videos=%v err=%v", videos, err)
			}
			if _, err := os.Stat(filepath.Join(dataDir, "frames", "session-1")); !os.IsNotExist(err) {
				t.Fatalf("frames should be removed, err=%v", err)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("session did not complete in time")
}

func TestParseLayer(t *testing.T) {
	tests := map[string]int{"12": 12, "12.0": 12, "Layer 7": 7, "unknown": 0, "0": 0}
	for input, want := range tests {
		if got := parseLayer(input); got != want {
			t.Fatalf("parseLayer(%q)=%d want %d", input, got, want)
		}
	}
}

func TestPollOnceCapturesWhileRunning(t *testing.T) {
	dataDir := t.TempDir()
	cfgStore, err := config.Open(filepath.Join(dataDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.HomeAssistant.Token = "token"
	cfg.HomeAssistant.StatusEntity = "sensor.printer_print_status"
	cfg.HomeAssistant.LayerEntity = "sensor.printer_current_layer"
	cfg.Trigger.Mode = config.TriggerLayer
	cfg.AI.Enabled = false
	if err := cfgStore.Update(cfg); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dataDir, "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	entities := fakeEntities{states: map[string]string{
		"sensor.printer_print_status":  "running",
		"sensor.printer_current_layer": "68",
	}}
	source := &fakeSource{data: testJPEG(t)}
	service := New(cfgStore, db, dataDir, source, entities, fakeNotifier{}, fakeDetector{}, fakeEncoder{})

	if err := service.pollOnce(context.Background()); err != nil {
		t.Fatalf("pollOnce err=%v", err)
	}
	active, err := db.ActiveSession()
	if err != nil || active == nil {
		t.Fatalf("期望 running 状态自动开始任务，active=%v err=%v", active, err)
	}
	frames, err := db.ListFrames(active.ID)
	if err != nil || len(frames) != 1 || frames[0].Layer != 68 {
		t.Fatalf("frames=%v err=%v", frames, err)
	}
}

func TestAnalyzeRespectsMinInterval(t *testing.T) {
	tests := []struct {
		name     string
		interval int
		want     int
	}{
		{"不限速时每层都分析", 0, 2},
		{"最小间隔内只分析一次", 60, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			cfgStore, err := config.Open(filepath.Join(dataDir, "config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			cfg := config.Default()
			cfg.HomeAssistant.Token = "token"
			cfg.Trigger.Mode = config.TriggerLayer
			cfg.AI.Enabled = true
			cfg.AI.APIKey = "key"
			cfg.AI.MaxChecksPerPrint = 0
			cfg.AI.MinIntervalSeconds = tc.interval
			if err := cfgStore.Update(cfg); err != nil {
				t.Fatal(err)
			}
			db, err := store.Open(filepath.Join(dataDir, "monitor.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			detector := &countingDetector{}
			service := New(cfgStore, db, dataDir, &fakeSource{data: testJPEG(t)}, fakeEntities{}, fakeNotifier{}, detector, fakeEncoder{})
			ctx := context.Background()
			if _, _, err := service.Start(ctx); err != nil {
				t.Fatal(err)
			}
			for layer := 1; layer <= 2; layer++ {
				if _, captured, err := service.CaptureLayer(ctx, layer); err != nil || !captured {
					t.Fatalf("layer %d captured=%v err=%v", layer, captured, err)
				}
			}
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) && detector.count() < tc.want {
				time.Sleep(10 * time.Millisecond)
			}
			time.Sleep(120 * time.Millisecond)
			if got := detector.count(); got != tc.want {
				t.Fatalf("AI 调用次数=%d，期望 %d", got, tc.want)
			}
		})
	}
}

func TestStatusClassification(t *testing.T) {
	for _, status := range []string{"printing", "running", "prepare", "preparing"} {
		if !statusIsPrinting(status) || !statusAllowsCapture(status) {
			t.Fatalf("status %q 应当视为打印中", status)
		}
	}
	for _, status := range []string{"idle", "finished", "finish", "failed", "fail", "stopped", "stop", "offline"} {
		if !statusIsStopped(status) {
			t.Fatalf("status %q 应当视为已结束", status)
		}
		if statusAllowsCapture(status) {
			t.Fatalf("status %q 不应当抓图", status)
		}
	}
	if !statusAllowsCapture("") {
		t.Fatal("未配置状态实体时应当允许抓图")
	}
	if statusAllowsCapture("paused") {
		t.Fatal("暂停时不应当抓图")
	}
}

func testJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 3, 3))
	img.Set(1, 1, color.RGBA{G: 255, A: 255})
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
