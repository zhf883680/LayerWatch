package ha

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhf883680/LayerWatch/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientSnapshotStateAndNotify(t *testing.T) {
	var notified map[string]any
	imageBytes := makeJPEG(t)
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("missing auth header")
		}
		status := http.StatusOK
		contentType := "application/json"
		var body []byte
		switch {
		case r.URL.Path == "/api/":
			contentType = "text/plain"
			body = []byte("API running")
		case r.URL.Path == "/api/camera_proxy/camera.a1":
			contentType = "image/jpeg"
			body = imageBytes
		case r.URL.Path == "/api/states/sensor.layer":
			body, _ = json.Marshal(EntityState{EntityID: "sensor.layer", State: "12"})
		case r.URL.Path == "/api/services/persistent_notification/create":
			if err := json.NewDecoder(r.Body).Decode(&notified); err != nil {
				t.Fatal(err)
			}
		default:
			status = http.StatusNotFound
			body = []byte("not found")
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{contentType}},
			Body:       io.NopCloser(bytes.NewReader(body)),
			Request:    r,
		}, nil
	})

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := config.Open(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	value := config.Default()
	value.HomeAssistant.BaseURL = "http://ha.test"
	value.HomeAssistant.Token = "Bearer token"
	value.HomeAssistant.CameraEntity = "camera.a1"
	value.AI.APIKey = "key"
	if err := cfg.Update(value); err != nil {
		t.Fatal(err)
	}
	client := New(cfg)
	client.client.Transport = transport
	if err := client.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(snapshot[:3], []byte{0xff, 0xd8, 0xff}) {
		t.Fatalf("snapshot is not JPEG")
	}
	state, err := client.EntityState(context.Background(), "sensor.layer")
	if err != nil || state != "12" {
		t.Fatalf("state=%q err=%v", state, err)
	}
	if err := client.Notify(context.Background(), "title", "message"); err != nil {
		t.Fatal(err)
	}
	if notified["title"] != "title" || notified["message"] != "message" {
		t.Fatalf("notification=%v", notified)
	}
	if !strings.HasPrefix(notified["notification_id"].(string), "layerwatch_") {
		t.Fatalf("notification id=%v", notified["notification_id"])
	}
}

func makeJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, nil); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
