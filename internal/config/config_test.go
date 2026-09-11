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

func TestNormalizeMinInterval(t *testing.T) {
	cfg := Default()
	cfg.AI.MinIntervalSeconds = -5
	cfg.Normalize()
	if cfg.AI.MinIntervalSeconds != 30 {
		t.Fatalf("负数应回落到默认值，得到 %d", cfg.AI.MinIntervalSeconds)
	}
	cfg.AI.MinIntervalSeconds = 99999
	cfg.Normalize()
	if cfg.AI.MinIntervalSeconds != 3600 {
		t.Fatalf("过大应被截断到 3600，得到 %d", cfg.AI.MinIntervalSeconds)
	}
	cfg.AI.MinIntervalSeconds = 0
	cfg.Normalize()
	if cfg.AI.MinIntervalSeconds != 0 {
		t.Fatalf("0 表示不限制，应保留，得到 %d", cfg.AI.MinIntervalSeconds)
	}
}

func TestNormalizeTokenSavingDefaults(t *testing.T) {
	cfg := Default()
	if cfg.AI.MaxImageWidth != 1280 {
		t.Fatalf("默认缩图宽度应为 1280，得到 %d", cfg.AI.MaxImageWidth)
	}
	if cfg.AI.ImageSource != ImageSourceBase64 {
		t.Fatalf("默认图片传输应为 base64，得到 %q", cfg.AI.ImageSource)
	}
}

func TestNormalizeTokenSavingOverrides(t *testing.T) {
	cfg := Default()
	cfg.AI.MaxImageWidth = -1
	cfg.AI.ImageSource = "TEMP"
	cfg.Normalize()
	if cfg.AI.MaxImageWidth != 1280 {
		t.Fatalf("负数应回落到默认宽度，得到 %d", cfg.AI.MaxImageWidth)
	}
	if cfg.AI.ImageSource != ImageSourceTemp {
		t.Fatalf("imageSource 应保留 temp，得到 %q", cfg.AI.ImageSource)
	}

	cfg.AI.MaxImageWidth = 0
	cfg.AI.ImageSource = "ftp"
	cfg.Normalize()
	if cfg.AI.MaxImageWidth != 0 {
		t.Fatalf("0 表示不压缩，应保留，得到 %d", cfg.AI.MaxImageWidth)
	}
	if cfg.AI.ImageSource != ImageSourceBase64 {
		t.Fatalf("非法 imageSource 应回落到 base64，得到 %q", cfg.AI.ImageSource)
	}

	cfg.AI.MaxImageWidth = 16
	cfg.Normalize()
	if cfg.AI.MaxImageWidth != 128 {
		t.Fatalf("过小的宽度应抬到 128，得到 %d", cfg.AI.MaxImageWidth)
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
