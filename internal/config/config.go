package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

const (
	TriggerLayer    = "layer"
	TriggerInterval = "interval"
)

// HomeAssistant 是用户显式指定的 Home Assistant 连接和实体。
// 不做自动发现，避免实体命名变化导致误抓其他摄像头。
type HomeAssistant struct {
	BaseURL      string `yaml:"baseURL" json:"baseURL"`
	Token        string `yaml:"token" json:"token"`
	CameraEntity string `yaml:"cameraEntity" json:"cameraEntity"`
	LayerEntity  string `yaml:"layerEntity" json:"layerEntity"`
	StatusEntity string `yaml:"statusEntity" json:"statusEntity"`
}

type AI struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	BaseURL string `yaml:"baseURL" json:"baseURL"`
	APIKey  string `yaml:"apiKey" json:"apiKey"`
	Model   string `yaml:"model" json:"model"`
	// MaxImageWidth 发给 AI 前把帧缩到该宽度（0=不压缩）。缩图是最稳定的省 token 手段。
	MaxImageWidth int `yaml:"maxImageWidth" json:"maxImageWidth"`
	// ImageSource 图片传输方式：base64（默认）| temp（阿里云百炼临时文件 URL，省请求体与带宽）。
	ImageSource       string `yaml:"imageSource" json:"imageSource"`
	MaxChecksPerPrint int    `yaml:"maxChecksPerPrint" json:"maxChecksPerPrint"`
	// MinIntervalSeconds 是两次 AI 分析之间的最小间隔，避免层号快速变化时连打 AI。0 表示不限制。
	MinIntervalSeconds int `yaml:"minIntervalSeconds" json:"minIntervalSeconds"`
}

const (
	// ImageSourceBase64 把图片编码成 data URL 塞进请求体。
	ImageSourceBase64 = "base64"
	// ImageSourceTemp 先传到阿里云百炼临时 OSS，再把 oss:// URL 发给模型。
	ImageSourceTemp = "temp"
)

type Trigger struct {
	Mode            string `yaml:"mode" json:"mode"`
	IntervalSeconds int    `yaml:"intervalSeconds" json:"intervalSeconds"`
}

type Cleanup struct {
	RetentionDays int `yaml:"retentionDays" json:"retentionDays"`
}

type Lighting struct {
	Enabled           bool   `yaml:"enabled" json:"enabled"`
	Entity            string `yaml:"entity" json:"entity"`
	DelaySeconds      int    `yaml:"delaySeconds" json:"delaySeconds"`
	KeepOnDuringPrint bool   `yaml:"keepOnDuringPrint" json:"keepOnDuringPrint"`
}

// Notification 同时支持 HA 持久通知和 Bark，可分别启用。
type Notification struct {
	HAEnabled   bool   `yaml:"haEnabled" json:"haEnabled"`
	BarkEnabled bool   `yaml:"barkEnabled" json:"barkEnabled"`
	BarkKey     string `yaml:"barkKey" json:"barkKey"`
	BarkBaseURL string `yaml:"barkBaseURL" json:"barkBaseURL"`
	BarkGroup   string `yaml:"barkGroup" json:"barkGroup"`
	BarkLevel   string `yaml:"barkLevel" json:"barkLevel"`
	BarkVolume  int    `yaml:"barkVolume" json:"barkVolume"`
}

type Config struct {
	HomeAssistant HomeAssistant `yaml:"homeAssistant" json:"homeAssistant"`
	AI            AI            `yaml:"ai" json:"ai"`
	Trigger       Trigger       `yaml:"trigger" json:"trigger"`
	Cleanup       Cleanup       `yaml:"cleanup" json:"cleanup"`
	Lighting      Lighting      `yaml:"lighting" json:"lighting"`
	Notification  Notification  `yaml:"notification" json:"notification"`
}

func Default() Config {
	return Config{
		HomeAssistant: HomeAssistant{
			BaseURL:      "http://homeassistant.local:8123",
			CameraEntity: "camera.bambu_lab_camera",
			LayerEntity:  "sensor.bambu_lab_current_layer",
			StatusEntity: "sensor.bambu_lab_print_status",
		},
		AI: AI{
			Enabled:            true,
			BaseURL:            "https://dashscope.aliyuncs.com/compatible-mode/v1",
			Model:              "qwen3-vl-flash",
			MaxImageWidth:      640,
			ImageSource:        ImageSourceBase64,
			MaxChecksPerPrint:  50,
			MinIntervalSeconds: 30,
		},
		Trigger: Trigger{Mode: TriggerLayer, IntervalSeconds: 10},
		Cleanup: Cleanup{RetentionDays: 7},
		Lighting: Lighting{
			Entity:            "light.bambu_lab_chamber_light",
			DelaySeconds:      3,
			KeepOnDuringPrint: true,
		},
		Notification: Notification{
			HAEnabled:   true,
			BarkBaseURL: "https://api.day.app",
			BarkGroup:   "LayerWatch",
		},
	}
}

func (c *Config) Normalize() {
	d := Default()
	c.HomeAssistant.BaseURL = strings.TrimRight(strings.TrimSpace(c.HomeAssistant.BaseURL), "/")
	if strings.HasSuffix(strings.ToLower(c.HomeAssistant.BaseURL), "/api") {
		c.HomeAssistant.BaseURL = strings.TrimSuffix(c.HomeAssistant.BaseURL, "/api")
	}
	c.HomeAssistant.Token = strings.TrimSpace(c.HomeAssistant.Token)
	c.HomeAssistant.CameraEntity = strings.TrimSpace(c.HomeAssistant.CameraEntity)
	c.HomeAssistant.LayerEntity = strings.TrimSpace(c.HomeAssistant.LayerEntity)
	c.HomeAssistant.StatusEntity = strings.TrimSpace(c.HomeAssistant.StatusEntity)
	if c.HomeAssistant.BaseURL == "" {
		c.HomeAssistant.BaseURL = d.HomeAssistant.BaseURL
	}
	if c.HomeAssistant.CameraEntity == "" {
		c.HomeAssistant.CameraEntity = d.HomeAssistant.CameraEntity
	}

	c.AI.BaseURL = strings.TrimRight(strings.TrimSpace(c.AI.BaseURL), "/")
	c.AI.APIKey = strings.TrimSpace(c.AI.APIKey)
	c.AI.Model = strings.TrimSpace(c.AI.Model)
	if c.AI.BaseURL == "" {
		c.AI.BaseURL = d.AI.BaseURL
	}
	if c.AI.Model == "" {
		c.AI.Model = d.AI.Model
	}
	if c.AI.MaxImageWidth < 0 {
		c.AI.MaxImageWidth = d.AI.MaxImageWidth
	}
	if c.AI.MaxImageWidth > 0 && c.AI.MaxImageWidth < 128 {
		// 太小的图模型看不清细节，反而更容易误判，统一抬到 128。
		c.AI.MaxImageWidth = 128
	}
	c.AI.ImageSource = strings.ToLower(strings.TrimSpace(c.AI.ImageSource))
	if c.AI.ImageSource != ImageSourceTemp {
		c.AI.ImageSource = ImageSourceBase64
	}
	if c.AI.MaxChecksPerPrint < 0 {
		c.AI.MaxChecksPerPrint = d.AI.MaxChecksPerPrint
	}
	if c.AI.MinIntervalSeconds < 0 {
		c.AI.MinIntervalSeconds = d.AI.MinIntervalSeconds
	}
	if c.AI.MinIntervalSeconds > 3600 {
		c.AI.MinIntervalSeconds = 3600
	}

	c.Trigger.Mode = strings.ToLower(strings.TrimSpace(c.Trigger.Mode))
	if c.Trigger.Mode != TriggerLayer && c.Trigger.Mode != TriggerInterval {
		c.Trigger.Mode = d.Trigger.Mode
	}
	if c.Trigger.IntervalSeconds <= 0 {
		c.Trigger.IntervalSeconds = d.Trigger.IntervalSeconds
	}
	if c.Trigger.IntervalSeconds < 2 {
		c.Trigger.IntervalSeconds = 2
	}
	if c.Cleanup.RetentionDays < 0 {
		c.Cleanup.RetentionDays = d.Cleanup.RetentionDays
	}

	c.Lighting.Entity = strings.TrimSpace(c.Lighting.Entity)
	if c.Lighting.Entity == "" {
		c.Lighting.Entity = d.Lighting.Entity
	}
	if c.Lighting.DelaySeconds < 0 {
		c.Lighting.DelaySeconds = d.Lighting.DelaySeconds
	}
	if c.Lighting.DelaySeconds > 30 {
		c.Lighting.DelaySeconds = 30
	}

	c.Notification.BarkKey = strings.TrimSpace(c.Notification.BarkKey)
	c.Notification.BarkBaseURL = strings.TrimRight(strings.TrimSpace(c.Notification.BarkBaseURL), "/")
	c.Notification.BarkGroup = strings.TrimSpace(c.Notification.BarkGroup)
	if c.Notification.BarkBaseURL == "" {
		c.Notification.BarkBaseURL = d.Notification.BarkBaseURL
	}
	if c.Notification.BarkGroup == "" {
		c.Notification.BarkGroup = d.Notification.BarkGroup
	}
	switch strings.ToLower(strings.TrimSpace(c.Notification.BarkLevel)) {
	case "active":
		c.Notification.BarkLevel = "active"
	case "timesensitive":
		c.Notification.BarkLevel = "timeSensitive"
	case "critical":
		c.Notification.BarkLevel = "critical"
	default:
		c.Notification.BarkLevel = ""
	}
	if c.Notification.BarkVolume < 0 || c.Notification.BarkVolume > 10 {
		c.Notification.BarkVolume = 0
	}
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.HomeAssistant.BaseURL) == "" {
		return errors.New("homeAssistant.baseURL 不能为空")
	}
	if strings.TrimSpace(c.HomeAssistant.Token) == "" {
		return errors.New("homeAssistant.token 不能为空")
	}
	if strings.TrimSpace(c.HomeAssistant.CameraEntity) == "" {
		return errors.New("homeAssistant.cameraEntity 不能为空")
	}
	if c.Trigger.Mode == TriggerLayer && strings.TrimSpace(c.HomeAssistant.LayerEntity) == "" {
		return errors.New("按层触发需要配置 homeAssistant.layerEntity")
	}
	if c.AI.Enabled {
		if strings.TrimSpace(c.AI.BaseURL) == "" {
			return errors.New("ai.baseURL 不能为空")
		}
		if strings.TrimSpace(c.AI.APIKey) == "" {
			return errors.New("ai.apiKey 不能为空")
		}
		if strings.TrimSpace(c.AI.Model) == "" {
			return errors.New("ai.model 不能为空")
		}
	}
	return nil
}

// Store 提供并发安全的运行时配置读写，并负责原子落盘。
type Store struct {
	path string
	mu   sync.RWMutex
	cfg  Config
}

func Open(path string) (*Store, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			return nil, fmt.Errorf("解析配置 %s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		// 首次启动使用默认值，由用户随后在页面填写 HA/AI 配置。
	default:
		return nil, fmt.Errorf("读取配置 %s: %w", path, err)
	}
	cfg.Normalize()
	return &Store{path: path, cfg: cfg}, nil
}

func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *Store) Update(cfg Config) error {
	cfg.Normalize()
	// 允许分步保存：检测按钮会在配置不完整时给出具体缺项提示。
	if err := writeAtomic(s.path, cfg); err != nil {
		return err
	}
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
	return nil
}

func (s *Store) Save() error {
	s.mu.RLock()
	cfg := s.cfg
	s.mu.RUnlock()
	return writeAtomic(s.path, cfg)
}

func writeAtomic(path string, cfg Config) error {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建配置目录: %w", err)
		}
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("序列化配置: %w", err)
	}
	header := []byte("# LayerWatch 配置\n")
	b = append(header, b...)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("写入临时配置: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("替换配置: %w", err)
	}
	return nil
}
