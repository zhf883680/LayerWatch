package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenDefaultsAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.Get()
	if cfg.Trigger.Mode != TriggerLayer {
		t.Fatalf("default trigger=%q", cfg.Trigger.Mode)
	}
	cfg.HomeAssistant.BaseURL = "http://ha.local:8123/"
	cfg.HomeAssistant.Token = "secret"
	cfg.AI.APIKey = "key"
	if err := store.Update(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.Get()
	if got.HomeAssistant.BaseURL != "http://ha.local:8123" {
		t.Fatalf("baseURL=%q", got.HomeAssistant.BaseURL)
	}
	if got.HomeAssistant.Token != "secret" {
		t.Fatalf("token not persisted")
	}
}

func TestValidateLayerEntity(t *testing.T) {
	cfg := Default()
	cfg.HomeAssistant.Token = "token"
	cfg.HomeAssistant.LayerEntity = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected layer entity validation error")
	}
}
