package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"time"

	"github.com/zhf883680/LayerWatch/internal/config"
)

type DiagnosticCheck struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

type DiagnosticResult struct {
	OK     bool              `json:"ok"`
	Checks []DiagnosticCheck `json:"checks"`
}

// TestHA 验证 HA 基础连接、摄像头和当前配置的实体。
func (s *Service) TestHA(ctx context.Context) DiagnosticResult {
	result := DiagnosticResult{OK: true}
	cfg := s.cfg.Get()

	add := func(name string, err error, success string) {
		check := DiagnosticCheck{Name: name, OK: err == nil}
		if err != nil {
			check.Message = err.Error()
			result.OK = false
		} else {
			check.Message = success
		}
		result.Checks = append(result.Checks, check)
	}

	checker, ok := s.source.(interface{ Check(context.Context) error })
	if !ok {
		add("HA API", errors.New("客户端不支持连接检测"), "")
	} else {
		add("HA API", checker.Check(ctx), "连接和 Token 正常")
	}

	if strings.TrimSpace(cfg.HomeAssistant.CameraEntity) == "" {
		add("摄像头实体", errors.New("未配置 cameraEntity"), "")
	} else if _, err := s.source.Snapshot(ctx); err != nil {
		add("摄像头实体", err, "")
	} else {
		add("摄像头实体", nil, cfg.HomeAssistant.CameraEntity+" 可获取快照")
	}

	if strings.TrimSpace(cfg.HomeAssistant.StatusEntity) != "" {
		state, err := s.entities.EntityState(ctx, cfg.HomeAssistant.StatusEntity)
		add("打印状态实体", err, fmt.Sprintf("%s = %s", cfg.HomeAssistant.StatusEntity, state))
	}

	if strings.TrimSpace(cfg.HomeAssistant.LayerEntity) != "" {
		state, err := s.entities.EntityState(ctx, cfg.HomeAssistant.LayerEntity)
		add("当前层实体", err, fmt.Sprintf("%s = %s", cfg.HomeAssistant.LayerEntity, state))
	} else if cfg.Trigger.Mode == config.TriggerLayer {
		add("当前层实体", errors.New("按层触发必须配置 layerEntity"), "")
	}
	return result
}

// TestAI 使用一张内置测试图真实调用 AI 端点，验证地址、API Key 和模型。
func (s *Service) TestAI(ctx context.Context) DiagnosticResult {
	result := DiagnosticResult{OK: true}
	if !s.detector.Enabled() {
		result.OK = false
		result.Checks = append(result.Checks, DiagnosticCheck{Name: "AI 配置", OK: false, Message: "AI 未启用或 baseURL、apiKey、model 未完整配置"})
		return result
	}
	img, err := testImage()
	if err != nil {
		result.OK = false
		result.Checks = append(result.Checks, DiagnosticCheck{Name: "测试图片", OK: false, Message: err.Error()})
		return result
	}
	reply, err := s.detector.Analyze(ctx, [][]byte{img})
	if err != nil {
		result.OK = false
		result.Checks = append(result.Checks, DiagnosticCheck{Name: "AI 请求", OK: false, Message: err.Error()})
		return result
	}
	if reply == nil {
		result.OK = false
		result.Checks = append(result.Checks, DiagnosticCheck{Name: "AI 请求", OK: false, Message: "AI 返回空结果"})
		return result
	}
	result.Checks = append(result.Checks, DiagnosticCheck{
		Name:    "AI 请求",
		OK:      true,
		Message: fmt.Sprintf("调用成功，测试结果=%s，置信度=%.0f%%", reply.Status, reply.Confidence*100),
	})
	return result
}

func testImage() ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{R: 210, G: 218, B: 225, A: 255})
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 80}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// TestNotification 同时测试当前启用的 HA 和 Bark 通知渠道。
func (s *Service) TestNotification(ctx context.Context) DiagnosticResult {
	result := DiagnosticResult{OK: true}
	if s.notifier == nil {
		result.OK = false
		result.Checks = append(result.Checks, DiagnosticCheck{Name: "通知发送", OK: false, Message: "通知客户端未配置"})
		return result
	}
	n := s.cfg.Get().Notification
	if !n.HAEnabled && !n.BarkEnabled {
		result.OK = false
		result.Checks = append(result.Checks, DiagnosticCheck{Name: "通知发送", OK: false, Message: "HA 和 Bark 均未启用"})
		return result
	}
	err := s.notifier.Notify(ctx, "LayerWatch 测试通知", "通知配置正常。发送时间："+time.Now().Format("2006-01-02 15:04:05"))
	check := DiagnosticCheck{Name: "通知发送", OK: err == nil}
	if err != nil {
		check.Message = err.Error()
		result.OK = false
	} else {
		check.Message = "已向所有启用的通知渠道发送测试消息"
	}
	result.Checks = append(result.Checks, check)
	return result
}
