package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/zhf883680/LayerWatch/internal/config"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type fakeHA struct{ called atomic.Bool }

func (f *fakeHA) Notify(context.Context, string, string) error {
	f.called.Store(true)
	return nil
}

func TestNotifySendsHANotificationAndBark(t *testing.T) {
	cfgStore, err := config.Open(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Notification.HAEnabled = true
	cfg.Notification.BarkEnabled = true
	cfg.Notification.BarkKey = "device-key"
	cfg.Notification.BarkBaseURL = "https://bark.test"
	cfg.Notification.BarkGroup = "打印机"
	cfg.Notification.BarkLevel = "critical"
	cfg.Notification.BarkVolume = 10
	if err := cfgStore.Update(cfg); err != nil {
		t.Fatal(err)
	}
	ha := &fakeHA{}
	service := New(cfgStore, ha)
	var payload barkPayload
	service.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://bark.test/push" {
			t.Fatalf("url=%s", r.URL)
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(nil)), Header: make(http.Header), Request: r}, nil
	})
	if err := service.Notify(context.Background(), "标题", "正文"); err != nil {
		t.Fatal(err)
	}
	if !ha.called.Load() {
		t.Fatal("HA notification was not sent")
	}
	if payload.DeviceKey != "device-key" || payload.Group != "打印机" || payload.Level != "critical" || payload.Volume != 10 {
		t.Fatalf("payload=%+v", payload)
	}
}
