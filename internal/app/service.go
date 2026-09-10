package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zhf883680/LayerWatch/internal/config"
	"github.com/zhf883680/LayerWatch/internal/ffmpeg"
	"github.com/zhf883680/LayerWatch/internal/store"
	"github.com/zhf883680/LayerWatch/internal/vision"
)

const (
	pollInterval     = 2 * time.Second
	cleanupInterval  = 6 * time.Hour
	analyzeFrames    = 5
	minAIConfidence  = 0.8
	failureStreak    = 3
	alertCooldown    = 5 * time.Minute
	maxSnapshotRetry = 1
)

type SnapshotSource interface {
	Snapshot(ctx context.Context) ([]byte, error)
}

type EntitySource interface {
	EntityState(ctx context.Context, entityID string) (string, error)
}

type Notifier interface {
	Notify(ctx context.Context, title, message string) error
}

type Detector interface {
	Enabled() bool
	Analyze(ctx context.Context, images [][]byte) (*vision.Result, error)
}

type Encoder interface {
	Encode(ctx context.Context, framesDir, outputPath string) error
	Binary() string
}

type Service struct {
	cfg      *config.Store
	db       *store.Store
	dataDir  string
	source   SnapshotSource
	entities EntitySource
	notifier Notifier
	detector Detector
	encoder  Encoder

	mu     sync.Mutex
	active *runtimeSession

	captureMu sync.Mutex
	aiMu      sync.Mutex
	aiStates  map[int64]*aiState

	lastLayer   map[int64]int
	lastLayerMu sync.Mutex

	lightMu       sync.Mutex
	lightSessions map[int64]bool
}

type runtimeSession struct {
	session store.Session
	cancel  context.CancelFunc
}

type aiState struct {
	running bool
	pending bool
}

func New(cfg *config.Store, db *store.Store, dataDir string, source SnapshotSource, entities EntitySource, notifier Notifier, detector Detector, encoder Encoder) *Service {
	return &Service{
		cfg:           cfg,
		db:            db,
		dataDir:       dataDir,
		source:        source,
		entities:      entities,
		notifier:      notifier,
		detector:      detector,
		encoder:       encoder,
		aiStates:      make(map[int64]*aiState),
		lastLayer:     make(map[int64]int),
		lightSessions: make(map[int64]bool),
	}
}

func (s *Service) Run(ctx context.Context) {
	go s.watchTriggers(ctx)
	go s.cleanupLoop(ctx)
}

func (s *Service) Start(ctx context.Context) (store.Session, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil {
		return s.active.session, true, nil
	}
	if active, err := s.db.ActiveSession(); err != nil {
		return store.Session{}, false, err
	} else if active != nil {
		// 数据库里若有孤立 running 任务，只接管为当前任务，避免重复创建。
		triggerCtx, cancel := context.WithCancel(context.Background())
		s.active = &runtimeSession{session: *active, cancel: cancel}
		if active.TriggerMode == config.TriggerInterval {
			go s.intervalLoop(triggerCtx, active.ID)
		}
		return *active, true, nil
	}
	cfg := s.cfg.Get()
	session, err := s.db.CreateSession(cfg.Trigger.Mode)
	if err != nil {
		return store.Session{}, false, err
	}
	triggerCtx, cancel := context.WithCancel(context.Background())
	s.active = &runtimeSession{session: session, cancel: cancel}
	if cfg.Trigger.Mode == config.TriggerInterval {
		go s.intervalLoop(triggerCtx, session.ID)
	}
	log.Printf("[session] 开始打印任务 #%d，触发模式=%s", session.ID, session.TriggerMode)
	return session, false, nil
}

func (s *Service) Stop(ctx context.Context) (store.Session, bool, error) {
	s.mu.Lock()
	active := s.active
	if active == nil {
		s.mu.Unlock()
		return store.Session{}, false, nil
	}
	s.active = nil
	active.cancel()
	s.mu.Unlock()

	// 等待正在写入的那一张结束后再进入编码，保证帧序列完整。
	s.captureMu.Lock()
	s.captureMu.Unlock()
	s.releaseSessionLight(active.session.ID)
	if err := s.db.SetSessionStatus(active.session.ID, store.SessionEncoding, ""); err != nil {
		return store.Session{}, false, err
	}
	session, err := s.db.GetSession(active.session.ID)
	if err != nil {
		return store.Session{}, false, err
	}
	log.Printf("[session] 任务 #%d 停止抓图，开始生成延时视频", session.ID)
	go s.finishSession(active.session.ID)
	return session, true, nil
}

func (s *Service) CaptureLayer(ctx context.Context, layer int) (store.Frame, bool, error) {
	cfg := s.cfg.Get()
	if cfg.Trigger.Mode != config.TriggerLayer {
		return store.Frame{}, false, nil
	}
	if layer <= 0 {
		return store.Frame{}, false, errors.New("layer 必须是正整数")
	}
	if _, err := s.ensureRunning(ctx); err != nil {
		return store.Frame{}, false, err
	}
	frame, captured, err := s.capture(ctx, layer)
	return frame, captured, err
}

func (s *Service) CaptureNow(ctx context.Context) (store.Frame, bool, error) {
	if _, err := s.ensureRunning(ctx); err != nil {
		return store.Frame{}, false, err
	}
	return s.capture(ctx, 0)
}

func (s *Service) ensureRunning(ctx context.Context) (store.Session, error) {
	s.mu.Lock()
	if s.active != nil {
		session := s.active.session
		s.mu.Unlock()
		return session, nil
	}
	s.mu.Unlock()
	session, _, err := s.Start(ctx)
	return session, err
}

func (s *Service) capture(ctx context.Context, layer int) (store.Frame, bool, error) {
	s.captureMu.Lock()
	defer s.captureMu.Unlock()

	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	if active == nil {
		return store.Frame{}, false, errors.New("当前没有进行中的打印任务")
	}
	sessionID := active.session.ID
	if layer > 0 {
		if exists, err := s.db.HasLayer(sessionID, layer); err != nil {
			return store.Frame{}, false, err
		} else if exists {
			return store.Frame{}, false, nil
		}
		if last := s.lastLayerValue(sessionID); layer == last {
			return store.Frame{}, false, nil
		}
	}
	releaseLight := s.prepareCaptureLight(ctx, sessionID)
	var data []byte
	var err error
	for attempt := 0; attempt <= maxSnapshotRetry; attempt++ {
		data, err = s.source.Snapshot(ctx)
		if err == nil {
			break
		}
		if attempt < maxSnapshotRetry {
			select {
			case <-ctx.Done():
				releaseLight()
				return store.Frame{}, false, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	releaseLight()
	if err != nil {
		return store.Frame{}, false, fmt.Errorf("抓取摄像头快照: %w", err)
	}
	count, err := s.db.CountFrames(sessionID)
	if err != nil {
		return store.Frame{}, false, err
	}
	frameNo := count + 1
	dir := s.framesDir(sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return store.Frame{}, false, err
	}
	path := filepath.Join(dir, fmt.Sprintf("%06d.jpg", frameNo))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return store.Frame{}, false, err
	}
	frame, err := s.db.AddFrame(sessionID, frameNo, layer, path)
	if err != nil {
		_ = os.Remove(path)
		return store.Frame{}, false, err
	}
	if layer > 0 {
		s.setLastLayer(sessionID, layer)
	}
	s.maybeAnalyze(sessionID)
	return frame, true, nil
}

func (s *Service) intervalLoop(ctx context.Context, sessionID int64) {
	cfg := s.cfg.Get()
	interval := time.Duration(cfg.Trigger.IntervalSeconds) * time.Second
	if interval < 2*time.Second {
		interval = 2 * time.Second
	}
	if _, _, err := s.capture(ctx, 0); err != nil {
		log.Printf("[session #%d] 首次定时抓图失败: %v", sessionID, err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.isActive(sessionID) {
				return
			}
			if !s.shouldCaptureByStatus(ctx) {
				continue
			}
			if _, _, err := s.capture(ctx, 0); err != nil {
				log.Printf("[session #%d] 定时抓图失败: %v", sessionID, err)
			}
		}
	}
}

func (s *Service) watchTriggers(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	failures := 0
	var nextAttempt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if time.Now().Before(nextAttempt) {
				continue
			}
			err := s.pollOnce(ctx)
			if err == nil {
				failures = 0
				nextAttempt = time.Time{}
				continue
			}
			failures++
			delay := triggerBackoff(failures)
			nextAttempt = time.Now().Add(delay)
			if failures == 1 || failures == 3 || failures%10 == 0 {
				log.Printf("[watch] HA 轮询失败: %v；%s 后重试", err, delay)
			}
		}
	}
}

func triggerBackoff(failures int) time.Duration {
	switch {
	case failures <= 1:
		return 5 * time.Second
	case failures == 2:
		return 10 * time.Second
	case failures == 3:
		return 30 * time.Second
	default:
		return 60 * time.Second
	}
}

func (s *Service) pollOnce(ctx context.Context) error {
	cfg := s.cfg.Get()
	if strings.TrimSpace(cfg.HomeAssistant.Token) == "" || strings.TrimSpace(cfg.HomeAssistant.BaseURL) == "" {
		return nil
	}
	var errs []error
	status := ""
	if cfg.HomeAssistant.StatusEntity != "" {
		value, err := s.entities.EntityState(ctx, cfg.HomeAssistant.StatusEntity)
		if err != nil {
			errs = append(errs, fmt.Errorf("读取打印状态: %w", err))
		} else {
			status = strings.ToLower(strings.TrimSpace(value))
		}
	}

	switch status {
	case "printing":
		if _, _, err := s.Start(ctx); err != nil {
			errs = append(errs, fmt.Errorf("自动开始任务: %w", err))
		}
	case "idle", "finished", "failed", "stopped", "offline":
		if _, found, err := s.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("自动停止任务: %w", err))
		} else if found {
			log.Printf("[watch] 打印状态=%s，已停止并开始出片", status)
		}
	}

	if cfg.Trigger.Mode != config.TriggerLayer || cfg.HomeAssistant.LayerEntity == "" {
		return errors.Join(errs...)
	}
	value, err := s.entities.EntityState(ctx, cfg.HomeAssistant.LayerEntity)
	if err != nil {
		errs = append(errs, fmt.Errorf("读取层数: %w", err))
		return errors.Join(errs...)
	}
	layer := parseLayer(value)
	if layer <= 0 {
		return errors.Join(errs...)
	}
	if status != "" && !statusAllowsCapture(status) {
		return errors.Join(errs...)
	}
	if _, captured, err := s.CaptureLayer(ctx, layer); err != nil {
		errs = append(errs, fmt.Errorf("层 %d 抓图: %w", layer, err))
	} else if captured {
		log.Printf("[watch] 层 %d 已抓图", layer)
	}
	return errors.Join(errs...)
}

func (s *Service) shouldCaptureByStatus(ctx context.Context) bool {
	cfg := s.cfg.Get()
	if cfg.HomeAssistant.StatusEntity == "" {
		return true
	}
	status, err := s.entities.EntityState(ctx, cfg.HomeAssistant.StatusEntity)
	if err != nil {
		return true
	}
	return statusAllowsCapture(strings.ToLower(strings.TrimSpace(status)))
}

func statusAllowsCapture(status string) bool {
	switch status {
	case "", "printing", "prepare", "preparing":
		return true
	default:
		return false
	}
}

func (s *Service) isActive(sessionID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active != nil && s.active.session.ID == sessionID
}

func (s *Service) lastLayerValue(sessionID int64) int {
	s.lastLayerMu.Lock()
	defer s.lastLayerMu.Unlock()
	return s.lastLayer[sessionID]
}

func (s *Service) setLastLayer(sessionID int64, layer int) {
	s.lastLayerMu.Lock()
	s.lastLayer[sessionID] = layer
	s.lastLayerMu.Unlock()
}

var numberPattern = regexp.MustCompile(`-?\d+(?:\.\d+)?`)

func parseLayer(value string) int {
	match := numberPattern.FindString(strings.TrimSpace(value))
	if match == "" {
		return 0
	}
	n, err := strconv.ParseFloat(match, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return int(n)
}

func (s *Service) maybeAnalyze(sessionID int64) {
	if !s.detector.Enabled() {
		return
	}
	cfg := s.cfg.Get()
	if cfg.AI.MaxChecksPerPrint > 0 {
		if n, err := s.db.CountChecks(sessionID); err == nil && n >= cfg.AI.MaxChecksPerPrint {
			return
		}
	}
	s.aiMu.Lock()
	state := s.aiStates[sessionID]
	if state == nil {
		state = &aiState{}
		s.aiStates[sessionID] = state
	}
	if state.running {
		state.pending = true
		s.aiMu.Unlock()
		return
	}
	state.running = true
	s.aiMu.Unlock()
	go s.aiLoop(sessionID)
}

func (s *Service) aiLoop(sessionID int64) {
	for {
		if cfg := s.cfg.Get(); cfg.AI.MaxChecksPerPrint > 0 {
			if n, err := s.db.CountChecks(sessionID); err == nil && n >= cfg.AI.MaxChecksPerPrint {
				break
			}
		}
		if err := s.analyzeOnce(sessionID); err != nil {
			log.Printf("[ai] 任务 #%d 分析失败: %v", sessionID, err)
		}
		s.aiMu.Lock()
		state := s.aiStates[sessionID]
		if state == nil || !state.pending {
			if state != nil {
				state.running = false
			}
			s.aiMu.Unlock()
			return
		}
		state.pending = false
		s.aiMu.Unlock()
	}
	s.aiMu.Lock()
	if state := s.aiStates[sessionID]; state != nil {
		state.running = false
		state.pending = false
	}
	s.aiMu.Unlock()
}

func (s *Service) analyzeOnce(sessionID int64) error {
	frames, err := s.db.ListFrames(sessionID)
	if err != nil {
		return err
	}
	if len(frames) == 0 {
		return nil
	}
	start := len(frames) - analyzeFrames
	if start < 0 {
		start = 0
	}
	images := make([][]byte, 0, len(frames)-start)
	for _, frame := range frames[start:] {
		data, err := os.ReadFile(frame.Path)
		if err == nil && len(data) > 0 {
			images = append(images, data)
		}
	}
	if len(images) == 0 {
		return errors.New("没有可读取的帧")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	result, err := s.detector.Analyze(ctx, images)
	if err != nil {
		return err
	}
	abnormal := result.Abnormal() && result.Confidence >= minAIConfidence
	streak := 1
	if abnormal {
		recent, _ := s.db.RecentChecks(sessionID, failureStreak+1)
		for _, check := range recent {
			if resultAbnormal(check.Status, check.Confidence) {
				streak++
				if streak >= failureStreak {
					break
				}
			} else {
				break
			}
		}
	}
	lastAlert, _ := s.db.LastAlertAt(sessionID)
	shouldAlert := abnormal && streak >= failureStreak && (lastAlert.IsZero() || time.Since(lastAlert) >= alertCooldown)

	imagePath := ""
	if result.Abnormal() {
		imagePath, err = s.saveEventImage(sessionID, images[len(images)-1], result.Status)
		if err != nil {
			log.Printf("[ai] 保存现场图失败: %v", err)
			imagePath = ""
		}
	}
	check, err := s.db.AddCheck(sessionID, result.Status, result.Confidence, result.Reason, imagePath, shouldAlert)
	if err != nil {
		return err
	}
	if shouldAlert {
		message := fmt.Sprintf("打印任务 #%d 检测到 %s（置信度 %.0f%%）\n%s", sessionID, statusLabel(result.Status), result.Confidence*100, result.Reason)
		notifyCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := s.notifier.Notify(notifyCtx, "LayerWatch 打印异常", message); err != nil {
			log.Printf("[ai] HA 通知失败: %v", err)
		} else {
			log.Printf("[ai] 已发送异常通知，检查记录 #%d", check.ID)
		}
	}
	return nil
}

func resultAbnormal(status string, confidence float64) bool {
	switch status {
	case vision.StatusSpaghetti, vision.StatusClog, vision.StatusObjectDisplaced, vision.StatusNozzleCollision, vision.StatusMaterialBuildup:
		return confidence >= minAIConfidence
	default:
		return false
	}
}

func statusLabel(status string) string {
	switch status {
	case vision.StatusSpaghetti:
		return "炒面"
	case vision.StatusClog:
		return "堵头/不出料"
	case vision.StatusObjectDisplaced:
		return "打印件位移"
	case vision.StatusNozzleCollision:
		return "喷嘴碰撞"
	case vision.StatusMaterialBuildup:
		return "喷嘴积料"
	case vision.StatusUnknown:
		return "无法判断"
	default:
		return "打印正常"
	}
}

func (s *Service) saveEventImage(sessionID int64, data []byte, status string) (string, error) {
	dir := filepath.Join(s.dataDir, "events", fmt.Sprintf("session-%d", sessionID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.jpg", time.Now().Format("20060102-150405.000"), status))
	return path, os.WriteFile(path, data, 0o644)
}

func (s *Service) finishSession(sessionID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	frames, err := s.db.ListFrames(sessionID)
	if err != nil {
		s.failSession(sessionID, err)
		return
	}
	if len(frames) == 0 {
		s.failSession(sessionID, errors.New("没有抓到任何帧，无法生成延时视频"))
		return
	}
	output := filepath.Join(s.dataDir, "videos", fmt.Sprintf("print-%d-%s.mp4", sessionID, time.Now().Format("20060102-150405")))
	if err := s.encoder.Encode(ctx, s.framesDir(sessionID), output); err != nil {
		_, _ = s.db.AddVideo(sessionID, output, 0, len(frames), ffmpeg.FPS(), "failed", err.Error())
		s.failSession(sessionID, err)
		return
	}
	info, err := os.Stat(output)
	if err != nil {
		s.failSession(sessionID, err)
		return
	}
	if _, err := s.db.AddVideo(sessionID, output, info.Size(), len(frames), ffmpeg.FPS(), "success", ""); err != nil {
		s.failSession(sessionID, err)
		return
	}
	if err := s.db.SetSessionStatus(sessionID, store.SessionComplete, ""); err != nil {
		log.Printf("[session #%d] 更新完成状态失败: %v", sessionID, err)
	}
	_ = os.RemoveAll(s.framesDir(sessionID))
	log.Printf("[session] 任务 #%d 延时视频已生成: %s", sessionID, output)
}

func (s *Service) failSession(sessionID int64, cause error) {
	log.Printf("[session #%d] 生成延时视频失败: %v", sessionID, cause)
	_ = s.db.SetSessionStatus(sessionID, store.SessionFailed, cause.Error())
}

func (s *Service) Status() map[string]any {
	active, _ := s.db.ActiveSession()
	latest, _ := s.db.LatestSession()
	var latestCheck *store.Check
	if active != nil {
		latestCheck, _ = s.db.LatestCheck(active.ID)
	} else if latest != nil {
		latestCheck, _ = s.db.LatestCheck(latest.ID)
	}
	return map[string]any{
		"active":      active,
		"latest":      latest,
		"latestCheck": latestCheck,
		"ffmpeg":      s.encoder.Binary(),
	}
}

func (s *Service) ListChecks(sessionID int64, limit int) ([]store.Check, error) {
	return s.db.ListChecks(sessionID, limit)
}

func (s *Service) ListVideos(limit int) ([]store.Video, error) {
	return s.db.ListVideos(limit)
}

func (s *Service) ListSessions(limit int) ([]store.Session, error) {
	return s.db.ListSessions(limit)
}

func (s *Service) Video(id int64) (store.Video, error) {
	return s.db.GetVideo(id)
}

func (s *Service) DeleteVideo(id int64) error {
	video, err := s.db.GetVideo(id)
	if err != nil {
		return err
	}
	if err := s.db.DeleteVideo(id); err != nil {
		return err
	}
	if video.FilePath != "" {
		_ = os.Remove(video.FilePath)
	}
	return nil
}

func (s *Service) CheckImagePath(id int64) (string, error) {
	check, err := s.db.GetCheck(id)
	if err != nil {
		return "", err
	}
	if check.ImagePath == "" {
		return "", errors.New("该分析没有现场图")
	}
	return check.ImagePath, nil
}

func (s *Service) CheckHA(ctx context.Context) error {
	if checker, ok := s.source.(interface{ Check(context.Context) error }); ok {
		return checker.Check(ctx)
	}
	return nil
}

func (s *Service) framesDir(sessionID int64) string {
	return filepath.Join(s.dataDir, "frames", fmt.Sprintf("session-%d", sessionID))
}

func (s *Service) ActiveSession() (*store.Session, error) {
	return s.db.ActiveSession()
}

func (s *Service) LatestSession() (*store.Session, error) {
	return s.db.LatestSession()
}

func (s *Service) LatestCheck(sessionID int64) (*store.Check, error) {
	return s.db.LatestCheck(sessionID)
}
