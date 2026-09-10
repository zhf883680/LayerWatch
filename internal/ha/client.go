package ha

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zhf883680/LayerWatch/internal/config"
)

const maxSnapshotBytes = 25 << 20

type EntityState struct {
	EntityID string `json:"entity_id"`
	State    string `json:"state"`
}

type Client struct {
	cfg    *config.Store
	client *http.Client
}

func New(cfg *config.Store) *Client {
	return &Client{
		cfg: cfg,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) Check(ctx context.Context) error {
	cfg := c.cfg.Get()
	req, err := c.request(ctx, http.MethodGet, "/api/", nil, cfg)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return responseError(resp)
	}
	return nil
}

func (c *Client) Snapshot(ctx context.Context) ([]byte, error) {
	cfg := c.cfg.Get()
	if strings.TrimSpace(cfg.HomeAssistant.CameraEntity) == "" {
		return nil, errors.New("未配置 homeAssistant.cameraEntity")
	}
	path := "/api/camera_proxy/" + url.PathEscape(strings.TrimSpace(cfg.HomeAssistant.CameraEntity))
	req, err := c.request(ctx, http.MethodGet, path, nil, cfg)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "image/*")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 HA 摄像头失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, responseError(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSnapshotBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取摄像头快照: %w", err)
	}
	if len(body) == 0 {
		return nil, errors.New("HA 返回空快照")
	}
	if len(body) > maxSnapshotBytes {
		return nil, fmt.Errorf("摄像头快照超过 %d MB", maxSnapshotBytes>>20)
	}
	return normalizeJPEG(body)
}

func (c *Client) EntityState(ctx context.Context, entityID string) (string, error) {
	entityID = strings.TrimSpace(entityID)
	if entityID == "" {
		return "", errors.New("实体 ID 为空")
	}
	cfg := c.cfg.Get()
	path := "/api/states/" + url.PathEscape(entityID)
	req, err := c.request(ctx, http.MethodGet, path, nil, cfg)
	if err != nil {
		return "", err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("读取 HA 实体 %s: %w", entityID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", responseError(resp)
	}
	var state EntityState
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&state); err != nil {
		return "", fmt.Errorf("解析 HA 实体 %s: %w", entityID, err)
	}
	return strings.TrimSpace(state.State), nil
}

func (c *Client) Notify(ctx context.Context, title, message string) error {
	cfg := c.cfg.Get()
	body := map[string]any{
		"title":           strings.TrimSpace(title),
		"message":         strings.TrimSpace(message),
		"notification_id": fmt.Sprintf("layerwatch_%d", time.Now().Unix()),
	}
	req, err := c.request(ctx, http.MethodPost, "/api/services/persistent_notification/create", body, cfg)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("发送 HA 通知: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return responseError(resp)
	}
	return nil
}

func (c *Client) request(ctx context.Context, method, path string, body any, cfg config.Config) (*http.Request, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.HomeAssistant.BaseURL), "/")
	token := normalizeToken(cfg.HomeAssistant.Token)
	if base == "" {
		return nil, errors.New("未配置 homeAssistant.baseURL")
	}
	if token == "" {
		return nil, errors.New("未配置 homeAssistant.token")
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "layerwatch-monitor/1.0")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func responseError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(b))
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("HA HTTP 403: %s（请检查 HA 用户权限、反向代理/WAF，以及 Token 是否被粘贴成 Bearer 前缀）", msg)
	}
	return fmt.Errorf("HA HTTP %d: %s", resp.StatusCode, msg)
}

func normalizeJPEG(data []byte) ([]byte, error) {
	if len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff {
		return data, nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("HA 摄像头返回的不是有效图片: %w", err)
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 90}); err != nil {
		return nil, fmt.Errorf("转换摄像头图片为 JPEG: %w", err)
	}
	return out.Bytes(), nil
}

func normalizeToken(raw string) string {
	value := strings.TrimSpace(raw)
	value = strings.Trim(value, `"'`)
	if len(value) >= 7 && strings.EqualFold(value[:7], "bearer ") {
		value = strings.TrimSpace(value[7:])
	}
	return value
}

// CallService 调用 Home Assistant 服务，例如 light.turn_on。
func (c *Client) CallService(ctx context.Context, domain, service string, data any) error {
	domain = strings.TrimSpace(domain)
	service = strings.TrimSpace(service)
	if domain == "" || service == "" {
		return errors.New("HA 服务名称不能为空")
	}
	cfg := c.cfg.Get()
	path := "/api/services/" + url.PathEscape(domain) + "/" + url.PathEscape(service)
	req, err := c.request(ctx, http.MethodPost, path, data, cfg)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("调用 HA 服务 %s.%s: %w", domain, service, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return responseError(resp)
	}
	return nil
}
