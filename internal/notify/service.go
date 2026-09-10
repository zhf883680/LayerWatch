package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/zhf883680/LayerWatch/internal/config"
)

type HANotifier interface {
	Notify(ctx context.Context, title, message string) error
}

type Service struct {
	cfg    *config.Store
	ha     HANotifier
	client *http.Client
}

func New(cfg *config.Store, ha HANotifier) *Service {
	return &Service{
		cfg:    cfg,
		ha:     ha,
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

// Notify 并行发送已启用的 HA 和 Bark 通知，单个渠道失败不影响另一个。
func (s *Service) Notify(ctx context.Context, title, message string) error {
	cfg := s.cfg.Get().Notification
	type sender func(context.Context) error
	var senders []sender
	if cfg.HAEnabled {
		senders = append(senders, func(ctx context.Context) error {
			if s.ha == nil {
				return errors.New("HA 通知客户端未配置")
			}
			return s.ha.Notify(ctx, title, message)
		})
	}
	if cfg.BarkEnabled {
		senders = append(senders, func(ctx context.Context) error {
			return s.sendBark(ctx, title, message)
		})
	}
	if len(senders) == 0 {
		return nil
	}
	errs := make([]error, len(senders))
	var wg sync.WaitGroup
	for i, send := range senders {
		wg.Add(1)
		go func(index int, fn sender) {
			defer wg.Done()
			errs[index] = fn(ctx)
		}(i, send)
	}
	wg.Wait()
	return errors.Join(errs...)
}

type barkPayload struct {
	DeviceKey string `json:"device_key"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Group     string `json:"group,omitempty"`
	Level     string `json:"level,omitempty"`
	Volume    int    `json:"volume,omitempty"`
}

func (s *Service) sendBark(ctx context.Context, title, message string) error {
	cfg := s.cfg.Get().Notification
	key := strings.TrimSpace(cfg.BarkKey)
	if key == "" {
		return errors.New("Bark 已启用但未填写设备 Key")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BarkBaseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.day.app"
	}
	payload := barkPayload{
		DeviceKey: key,
		Title:     strings.TrimSpace(title),
		Body:      strings.TrimSpace(message),
		Group:     strings.TrimSpace(cfg.BarkGroup),
		Level:     strings.TrimSpace(cfg.BarkLevel),
		Volume:    cfg.BarkVolume,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("Bark 请求编码失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/push", bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("Bark 请求创建失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", "layerwatch-monitor/1.0")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("Bark 请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("Bark HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
