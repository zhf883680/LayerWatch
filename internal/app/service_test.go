package app

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
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

type fakeEntities struct{}

func (fakeEntities) EntityState(context.Context, string) (string, error) { return "", nil }

type fakeNotifier struct{}

func (fakeNotifier) Notify(context.Context, string, string) error { return nil }

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
