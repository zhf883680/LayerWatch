package app

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zhf883680/LayerWatch/internal/config"
	"github.com/zhf883680/LayerWatch/internal/vision"
)

type diagnosticDetector struct{}

func (diagnosticDetector) Enabled() bool { return true }
func (diagnosticDetector) Analyze(context.Context, [][]byte) (*vision.Result, error) {
	return &vision.Result{Status: vision.StatusNormal, Confidence: 0.95, Reason: "测试正常"}, nil
}

func TestDiagnostics(t *testing.T) {
	cfgStore, err := config.Open(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.HomeAssistant.Token = "token"
	cfg.AI.APIKey = "key"
	if err := cfgStore.Update(cfg); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		cfg:      cfgStore,
		source:   &fakeSource{data: testJPEG(t)},
		entities: fakeEntities{},
		detector: diagnosticDetector{},
	}
	haResult := service.TestHA(context.Background())
	if !haResult.OK {
		t.Fatalf("HA diagnostics failed: %+v", haResult)
	}
	aiResult := service.TestAI(context.Background())
	if !aiResult.OK || len(aiResult.Checks) != 1 {
		t.Fatalf("AI diagnostics failed: %+v", aiResult)
	}
}
