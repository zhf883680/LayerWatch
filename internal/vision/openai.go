package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/zhf883680/LayerWatch/internal/config"
)

var ErrNotConfigured = errors.New("AI 未配置或未启用")
var jsonFence = regexp.MustCompile("(?s)```(?:json)?\\s*|\\s*```")

type Service struct {
	cfg    *config.Store
	client *http.Client
}

func New(cfg *config.Store) *Service {
	return &Service{cfg: cfg, client: &http.Client{Timeout: 180 * time.Second}}
}

func (s *Service) Enabled() bool {
	c := s.cfg.Get().AI
	return c.Enabled && strings.TrimSpace(c.BaseURL) != "" && strings.TrimSpace(c.APIKey) != "" && strings.TrimSpace(c.Model) != ""
}

type chatMessage struct {
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type chatRequest struct {
	Model          string        `json:"model"`
	Messages       []chatMessage `json:"messages"`
	EnableThinking *bool         `json:"enable_thinking,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (s *Service) Analyze(ctx context.Context, images [][]byte) (*Result, error) {
	if !s.Enabled() {
		return nil, ErrNotConfigured
	}
	if len(images) == 0 {
		return nil, errors.New("没有可分析的图片")
	}
	cfg := s.cfg.Get().AI
	parts := []contentPart{{Type: "text", Text: userPrompt}}
	for _, img := range images {
		if len(img) == 0 {
			continue
		}
		scaled, err := scaleMaxWidth(img, 1280)
		if err != nil {
			scaled = img
		}
		parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURL{URL: dataURL(scaled), Detail: "auto"}})
	}
	body := chatRequest{
		Model: cfg.Model,
		Messages: []chatMessage{
			{Role: "system", Content: []contentPart{{Type: "text", Text: systemPrompt}}},
			{Role: "user", Content: parts},
		},
	}
	if isDashScope(cfg.BaseURL) {
		off := false
		body.EnableThinking = &off
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "layerwatch-monitor/1.0")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("AI 请求失败: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	var parsed chatResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("AI 响应不是 JSON: %w", err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("AI 返回错误: %s", parsed.Error.Message)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("AI HTTP %d: %s", resp.StatusCode, truncate(string(data), 300))
	}
	if len(parsed.Choices) == 0 || strings.TrimSpace(parsed.Choices[0].Message.Content) == "" {
		return nil, errors.New("AI 返回内容为空")
	}
	return parseResult(parsed.Choices[0].Message.Content)
}

const systemPrompt = `你是 3D 打印监控助手。你会收到同一路摄像头、同一场打印的连续截图（从旧到新）。
请综合判断是否出现真实打印异常。打印头移动、遮挡、光影变化、轻微角度变化都属于正常现象，不能仅凭这些判异常。
判断位移必须依据热床、夹具、平台边缘等固定参照物；不确定时返回 normal。`

const userPrompt = `观察是否正常成型，重点识别炒面、堵头/严重积料、打印件脱离原位、喷嘴碰撞。
只输出 JSON，不要输出其他内容：
{"status":"normal|spaghetti|clog|object_displaced|nozzle_collision|material_buildup|unknown","confidence":0到1,"reason":"中文一句话"}
拿不准时选择 normal。`

func parseResult(content string) (*Result, error) {
	cleaned := jsonFence.ReplaceAllString(content, "")
	start, end := strings.Index(cleaned, "{"), strings.LastIndex(cleaned, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("AI 回复中没有 JSON: %s", truncate(content, 200))
	}
	var raw struct {
		Status     string  `json:"status"`
		Confidence float64 `json:"confidence"`
		Reason     string  `json:"reason"`
	}
	if err := json.Unmarshal([]byte(cleaned[start:end+1]), &raw); err != nil {
		return nil, fmt.Errorf("AI 回复 JSON 无效: %w", err)
	}
	status := strings.ToLower(strings.TrimSpace(raw.Status))
	if !KnownStatus(status) {
		status = StatusUnknown
	}
	confidence := raw.Confidence
	if confidence < 0 {
		confidence = 0
	}
	if confidence > 1 {
		confidence = 1
	}
	return &Result{Status: status, Confidence: confidence, Reason: strings.TrimSpace(raw.Reason)}, nil
}

func dataURL(b []byte) string {
	return "data:" + sniffImageMIME(b) + ";base64," + base64.StdEncoding.EncodeToString(b)
}

func sniffImageMIME(b []byte) string {
	switch {
	case len(b) >= 3 && b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff:
		return "image/jpeg"
	case len(b) >= 8 && bytes.Equal(b[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}):
		return "image/png"
	case len(b) >= 12 && bytes.Equal(b[0:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

func isDashScope(baseURL string) bool {
	v := strings.ToLower(baseURL)
	return strings.Contains(v, "dashscope.aliyuncs.com") || strings.Contains(v, "maas.aliyuncs.com")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
