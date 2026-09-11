package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/zhf883680/LayerWatch/internal/config"
)

var ErrNotConfigured = errors.New("AI 未配置或未启用")
var jsonFence = regexp.MustCompile("(?s)```(?:json)?\\s*|\\s*```")

// imageDetail 是固定的图片细节档位。auto 让模型自己按需取细节，
// 相比 low 保留识别小瑕疵的能力，相比 high/original 又不至于 token 失控。
const imageDetail = "auto"

type Service struct {
	cfg    *config.Store
	client *http.Client

	upMu     sync.Mutex
	uploader *tempUploader
	upKey    string
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
	Usage *usageInfo `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// usageInfo 一次调用的 token 用量，用于观察图片 token 与上下文缓存命中（缓存命中输入单价通常只有 1/10）。
type usageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	PromptDetails    struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (s *Service) Analyze(ctx context.Context, images [][]byte) (*Result, error) {
	if !s.Enabled() {
		return nil, ErrNotConfigured
	}
	if len(images) == 0 {
		return nil, errors.New("没有可分析的图片")
	}
	cfg := s.cfg.Get().AI
	uploader := s.tempUploader(cfg)
	useOSS := false
	parts := []contentPart{{Type: "text", Text: userPrompt}}
	for _, img := range images {
		if len(img) == 0 {
			continue
		}
		// 缩图是省 token 最稳定的一招：视觉模型按像素折算 token，先缩再发。
		if cfg.MaxImageWidth > 0 {
			if scaled, err := scaleMaxWidth(img, cfg.MaxImageWidth); err == nil {
				img = scaled
			}
		}
		url := dataURL(img)
		if uploader != nil {
			// 上传失败就回退 base64，宁可多花点 token 也不能丢帧。
			if u, err := uploader.upload(img); err == nil {
				url = u
				useOSS = true
			} else {
				log.Printf("[ai] 上传临时文件失败，回退 base64: %v", err)
			}
		}
		parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURL{URL: url, Detail: imageDetail}})
	}
	body := chatRequest{
		Model: cfg.Model,
		Messages: []chatMessage{
			{Role: "system", Content: []contentPart{{Type: "text", Text: systemPrompt}}},
			{Role: "user", Content: parts},
		},
	}
	// qwen3 等千问系列默认开思考模式（慢且 thinking token 计费），对阿里云固定关掉。
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
	if useOSS {
		// 用 oss:// 临时文件 URL 时必须带此头，否则百炼无法解析。
		req.Header.Set("X-DashScope-OssResourceResolve", "enable")
	}
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
	if u := parsed.Usage; u != nil {
		log.Printf("[ai] token 用量: 输入=%d 输出=%d 缓存命中=%d", u.PromptTokens, u.CompletionTokens, u.PromptDetails.CachedTokens)
	}
	return parseResult(parsed.Choices[0].Message.Content)
}

// tempUploader 仅在 imageSource=temp 且端点支持百炼临时文件时创建；
// baseURL/apiKey/model 变了就重建，避免拿旧模型的上传通道发图。
func (s *Service) tempUploader(cfg config.AI) *tempUploader {
	if cfg.ImageSource != config.ImageSourceTemp || !isDashScope(cfg.BaseURL) {
		return nil
	}
	key := cfg.BaseURL + "|" + cfg.APIKey + "|" + cfg.Model
	s.upMu.Lock()
	defer s.upMu.Unlock()
	if s.uploader != nil && s.upKey == key {
		return s.uploader
	}
	s.uploader = newTempUploader(cfg.BaseURL, cfg.APIKey, cfg.Model, s.client)
	s.upKey = key
	return s.uploader
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
