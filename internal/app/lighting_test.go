package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zhf883680/LayerWatch/internal/config"
	"github.com/zhf883680/LayerWatch/internal/store"
)

type fakeLightController struct {
	state string
	calls []string
	data  []byte
}

func (f *fakeLightController) Check(context.Context) error              { return nil }
func (f *fakeLightController) Snapshot(context.Context) ([]byte, error) { return f.data, nil }
func (f *fakeLightController) EntityState(context.Context, string) (string, error) {
	return f.state, nil
}
func (f *fakeLightController) CallService(_ context.Context, domain, service string, _ any) error {
	f.calls = append(f.calls, domain+"."+service)
	if service == "turn_on" {
		f.state = "on"
	} else if service == "turn_off" {
		f.state = "off"
	}
	return nil
}

func TestLightingDuringCaptureAndStop(t *testing.T) {
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
	cfg.Lighting = config.Lighting{Enabled: true, Entity: "light.test", DelaySeconds: 0, KeepOnDuringPrint: true}
	if err := cfgStore.Update(cfg); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(filepath.Join(dataDir, "monitor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source := &fakeLightController{state: "off", data: testJPEG(t)}
	service := New(cfgStore, db, dataDir, source, source, fakeNotifier{}, fakeDetector{}, fakeEncoder{})
	if _, _, err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, captured, err := service.CaptureLayer(context.Background(), 1); err != nil || !captured {
		t.Fatalf("capture failed: captured=%v err=%v", captured, err)
	}
	if source.state != "on" {
		t.Fatalf("light state=%q, want on", source.state)
	}
	if len(source.calls) != 1 || source.calls[0] != "light.turn_on" {
		t.Fatalf("calls=%v", source.calls)
	}
	if _, _, err := service.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if source.state != "off" {
		t.Fatalf("light state after stop=%q, want off", source.state)
	}
}
