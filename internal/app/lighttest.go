package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// 照明测试：点一次按钮，按设定的延迟点亮灯并连续抓图，
// 测出「开灯以后多久才能截到已经变亮的画面」。
const (
	lightTestDefaultIntervalMs = 300
	lightTestMinIntervalMs     = 100
	lightTestMaxIntervalMs     = 5000
	lightTestDefaultTimeoutMs  = 20000
	lightTestMinTimeoutMs      = 1000
	lightTestMaxTimeoutMs      = 120000
	lightTestMaxDelayMs        = 60000
	lightTestMaxFrames         = 400
	lightTestBaselineFrames    = 2
	lightTestThumbWidth        = 640
	lightTestKeepRuns          = 8
	lightTestMinDelta          = 4.0
	lightTestDeltaRatio        = 0.25
	lightTestMaxDelta          = 15.0
)

const (
	// LightTestRunning 测试进行中。
	LightTestRunning = "running"
	// LightTestDone 测试正常结束（无论是否截到变亮画面）。
	LightTestDone = "done"
	// LightTestFailed 测试因配置或 HA 调用失败中断。
	LightTestFailed = "failed"
	// LightTestCanceled 用户取消了测试。
	LightTestCanceled = "canceled"
)

var (
	lightTestRunPattern  = regexp.MustCompile(`^\d{8}-\d{6}\.\d{3}$`)
	lightTestFilePattern = regexp.MustCompile(`^frame-\d{3}\.jpg$`)
)

// LightTestOptions 是一轮照明测试的参数。
// DelayMs 对应真实抓图流程里的 lighting.delaySeconds：开灯后先等这么久再抓第一张。
type LightTestOptions struct {
	DelayMs      int  `json:"delayMs"`
	IntervalMs   int  `json:"intervalMs"`
	TimeoutMs    int  `json:"timeoutMs"`
	LeaveLightOn bool `json:"leaveLightOn"`
}

// LightTestFrame 是测试过程中抓到的单张画面。
type LightTestFrame struct {
	Index      int     `json:"index"`
	AfterMs    int64   `json:"afterMs"`
	Brightness float64 `json:"brightness"`
	SnapshotMs int64   `json:"snapshotMs"`
	Bright     bool    `json:"bright"`
	Baseline   bool    `json:"baseline"`
	ImageUrl   string  `json:"imageUrl,omitempty"`
}

// LightTestResult 是一次照明测试的完整结果，前端直接渲染。
type LightTestResult struct {
	ID           string     `json:"id"`
	Status       string     `json:"status"`
	Message      string     `json:"message,omitempty"`
	Advice       string     `json:"advice,omitempty"`
	Warning      string     `json:"warning,omitempty"`
	RestoreError string     `json:"restoreError,omitempty"`
	Entity       string     `json:"entity"`
	StartedAt    time.Time  `json:"startedAt"`
	FinishedAt   *time.Time `json:"finishedAt,omitempty"`
	DurationMs   int64      `json:"durationMs"`

	DelayMs      int  `json:"delayMs"`
	IntervalMs   int  `json:"intervalMs"`
	TimeoutMs    int  `json:"timeoutMs"`
	LeaveLightOn bool `json:"leaveLightOn"`

	InitialState  string `json:"initialState"`
	RestoredState string `json:"restoredState,omitempty"`

	// BaselineBrightness 是开灯前的平均亮度，Threshold 是判定「已经变亮」的门槛。
	BaselineBrightness float64 `json:"baselineBrightness"`
	Threshold          float64 `json:"threshold"`
	MaxBrightness      float64 `json:"maxBrightness"`
	// TurnOnMs 是 HA light.turn_on 这次调用本身的耗时。
	TurnOnMs int64 `json:"turnOnMs"`
	// DetectMs 是从发出开灯指令到第一张「已经变亮」的截图，单位毫秒；没测到则为空。
	DetectMs *int64 `json:"detectMs,omitempty"`
	// DelaySufficient 表示按设定延迟抓到的第一张图是否已经变亮。
	DelaySufficient *bool `json:"delaySufficient,omitempty"`

	Attempts       int              `json:"attempts"`
	SnapshotErrors int              `json:"snapshotErrors"`
	Frames         []LightTestFrame `json:"frames"`

	BaselineImage string `json:"baselineImage,omitempty"`
	BrightImage   string `json:"brightImage,omitempty"`
	LastImage     string `json:"lastImage,omitempty"`
}

type lightTestRun struct {
	id     string
	dir    string
	cancel context.CancelFunc
	mu     sync.Mutex
	result LightTestResult
}

func (r *lightTestRun) update(fn func(*LightTestResult)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(&r.result)
}

func (r *lightTestRun) appendFrame(frame LightTestFrame) {
	r.update(func(res *LightTestResult) { res.Frames = append(res.Frames, frame) })
}

func (r *lightTestRun) snapshot() LightTestResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	res := r.result
	res.Frames = append([]LightTestFrame(nil), r.result.Frames...)
	return res
}

func (r *lightTestRun) isRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.result.Status == LightTestRunning
}

func (r *lightTestRun) finish(status, message string) {
	r.update(func(res *LightTestResult) {
		res.Status = status
		if message != "" {
			res.Message = message
		}
		now := time.Now()
		res.FinishedAt = &now
		res.DurationMs = now.Sub(res.StartedAt).Milliseconds()
	})
}

// StartLightTest 启动一轮照明测试并立即返回当前进度，真正的测量在后台进行。
func (s *Service) StartLightTest(opts LightTestOptions) (*LightTestResult, error) {
	lightCfg := s.cfg.Get().Lighting
	entity := strings.TrimSpace(lightCfg.Entity)
	if entity == "" {
		return nil, errors.New("未配置照明实体（lighting.entity）")
	}
	caller, ok := s.source.(ServiceCaller)
	if !ok {
		return nil, errors.New("当前 HA 客户端不支持服务调用")
	}
	if s.entities == nil {
		return nil, errors.New("HA 实体客户端未配置")
	}
	opts = normalizeLightTestOptions(opts, lightCfg.DelaySeconds)

	s.lightTestMu.Lock()
	if run := s.lightTest; run != nil && run.isRunning() {
		s.lightTestMu.Unlock()
		return nil, errors.New("已有照明测试正在进行，请等待结束或先取消")
	}
	startedAt := time.Now()
	id := startedAt.Format("20060102-150405.000")
	dir := filepath.Join(s.dataDir, "light-tests", id)
	if s.lightTestDirs == nil {
		s.lightTestDirs = make(map[string]string)
	}
	s.lightTestDirs[id] = dir
	run := &lightTestRun{id: id, dir: dir}
	run.result = LightTestResult{
		ID:           id,
		Status:       LightTestRunning,
		Entity:       entity,
		StartedAt:    startedAt,
		DelayMs:      opts.DelayMs,
		IntervalMs:   opts.IntervalMs,
		TimeoutMs:    opts.TimeoutMs,
		LeaveLightOn: opts.LeaveLightOn,
		Frames:       []LightTestFrame{},
	}
	if s.db != nil {
		if active, err := s.db.ActiveSession(); err == nil && active != nil {
			run.result.Warning = fmt.Sprintf("当前有打印任务 #%d 在进行，测试会切换照明开关，可能影响正在抓取的帧。", active.ID)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(opts.TimeoutMs)*time.Millisecond+45*time.Second)
	run.cancel = cancel
	s.lightTest = run
	s.lightTestMu.Unlock()

	s.pruneLightTests(lightTestKeepRuns)
	go s.runLightTest(ctx, run, caller, entity, opts)
	result := run.snapshot()
	return &result, nil
}

// LightTestStatus 返回最近一次照明测试的进度；从未测试时返回 nil。
func (s *Service) LightTestStatus() *LightTestResult {
	s.lightTestMu.Lock()
	run := s.lightTest
	s.lightTestMu.Unlock()
	if run == nil {
		return nil
	}
	result := run.snapshot()
	return &result
}

// CancelLightTest 取消正在进行的测试。
func (s *Service) CancelLightTest() bool {
	s.lightTestMu.Lock()
	run := s.lightTest
	s.lightTestMu.Unlock()
	if run == nil || !run.isRunning() {
		return false
	}
	if run.cancel != nil {
		run.cancel()
	}
	return true
}

// LightTestImagePath 返回某次测试某张截图的磁盘路径；runID 与文件名都要严格匹配。
func (s *Service) LightTestImagePath(runID, name string) (string, error) {
	if !lightTestRunPattern.MatchString(runID) || !lightTestFilePattern.MatchString(name) {
		return "", errors.New("无效的照明测试图片")
	}
	s.lightTestMu.Lock()
	dir, ok := s.lightTestDirs[runID]
	s.lightTestMu.Unlock()
	if !ok {
		return "", errors.New("测试记录不存在或已清理")
	}
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err != nil {
		return "", errors.New("测试截图不存在")
	}
	return path, nil
}

func normalizeLightTestOptions(opts LightTestOptions, cfgDelaySeconds int) LightTestOptions {
	if opts.DelayMs < 0 {
		opts.DelayMs = cfgDelaySeconds * 1000
	}
	if opts.DelayMs < 0 {
		opts.DelayMs = 0
	}
	if opts.DelayMs > lightTestMaxDelayMs {
		opts.DelayMs = lightTestMaxDelayMs
	}
	if opts.IntervalMs <= 0 {
		opts.IntervalMs = lightTestDefaultIntervalMs
	}
	if opts.IntervalMs < lightTestMinIntervalMs {
		opts.IntervalMs = lightTestMinIntervalMs
	}
	if opts.IntervalMs > lightTestMaxIntervalMs {
		opts.IntervalMs = lightTestMaxIntervalMs
	}
	if opts.TimeoutMs <= 0 {
		opts.TimeoutMs = lightTestDefaultTimeoutMs
	}
	if opts.TimeoutMs < lightTestMinTimeoutMs {
		opts.TimeoutMs = lightTestMinTimeoutMs
	}
	if opts.TimeoutMs > lightTestMaxTimeoutMs {
		opts.TimeoutMs = lightTestMaxTimeoutMs
	}
	return opts
}

func (s *Service) runLightTest(ctx context.Context, run *lightTestRun, caller ServiceCaller, entity string, opts LightTestOptions) {
	// 1. 先确认灯的初始状态：要测「开灯后多久变亮」，就必须先有暗底。
	initialState, err := s.entities.EntityState(ctx, entity)
	if err != nil {
		run.finish(LightTestFailed, fmt.Sprintf("读取照明实体失败: %v", err))
		return
	}
	initialOn := lightIsOn(initialState)
	if !initialOn && !lightIsOff(initialState) {
		run.update(func(res *LightTestResult) { res.InitialState = initialState })
		run.finish(LightTestFailed, fmt.Sprintf("照明实体状态为 %q，无法确认开关状态", initialState))
		return
	}
	run.update(func(res *LightTestResult) { res.InitialState = initialState })

	// 灯原本就亮着时先关掉才能拿到暗底；测试结束后会恢复为亮。
	if initialOn {
		if err := caller.CallService(ctx, "light", "turn_off", map[string]any{"entity_id": entity}); err != nil {
			run.finish(LightTestFailed, fmt.Sprintf("测试前关灯失败: %v", err))
			return
		}
		if !s.waitLightState(ctx, entity, false, 5*time.Second) {
			log.Printf("[lighttest] 关灯后状态未在 5 秒内变为 off，继续测试")
		}
		lightTestSleep(ctx, 800*time.Millisecond)
	}

	// 2. 抓基线画面，用较亮的一张当暗底，判定门槛才不会偏松。
	baseline := 0.0
	baselineImage := ""
	for i := 0; i < lightTestBaselineFrames; i++ {
		if i > 0 && !lightTestSleep(ctx, 400*time.Millisecond) {
			s.finishLightTestCanceled(run, caller, entity, initialOn, opts.LeaveLightOn)
			return
		}
		data, snapshotMs, err := s.lightTestSnapshot(ctx)
		if err != nil {
			if ctx.Err() != nil {
				s.finishLightTestCanceled(run, caller, entity, initialOn, opts.LeaveLightOn)
				return
			}
			run.finish(LightTestFailed, fmt.Sprintf("抓取基线画面失败: %v", err))
			s.restoreLightTest(run, caller, entity, initialOn, opts.LeaveLightOn)
			return
		}
		brightness, err := imageBrightness(data)
		if err != nil {
			run.finish(LightTestFailed, fmt.Sprintf("分析基线画面失败: %v", err))
			s.restoreLightTest(run, caller, entity, initialOn, opts.LeaveLightOn)
			return
		}
		frame, saveErr := s.saveLightTestFrame(run, data, LightTestFrame{
			AfterMs:    0,
			Brightness: brightness,
			SnapshotMs: snapshotMs,
			Baseline:   true,
		})
		if saveErr != nil {
			log.Printf("[lighttest] 保存基线截图失败: %v", saveErr)
		}
		run.appendFrame(frame)
		if i == 0 || brightness > baseline {
			baseline = brightness
			if frame.ImageUrl != "" {
				baselineImage = frame.ImageUrl
			}
		}
	}
	threshold := lightTestThreshold(baseline)
	run.update(func(res *LightTestResult) {
		res.BaselineBrightness = baseline
		res.Threshold = threshold
		res.MaxBrightness = baseline
		res.BaselineImage = baselineImage
	})

	// 3. 开灯，按设定延迟抓第一张，之后按间隔连续抓到画面变亮或超时。
	turnOnStart := time.Now()
	if err := caller.CallService(ctx, "light", "turn_on", map[string]any{"entity_id": entity}); err != nil {
		run.finish(LightTestFailed, fmt.Sprintf("开灯失败: %v", err))
		s.restoreLightTest(run, caller, entity, initialOn, opts.LeaveLightOn)
		return
	}
	run.update(func(res *LightTestResult) { res.TurnOnMs = time.Since(turnOnStart).Milliseconds() })

	interval := time.Duration(opts.IntervalMs) * time.Millisecond
	deadline := turnOnStart.Add(time.Duration(opts.TimeoutMs) * time.Millisecond)
	next := turnOnStart.Add(time.Duration(opts.DelayMs) * time.Millisecond)

	var (
		detectMs         *int64
		firstAfterBright *bool
		brightImage      string
		lastImage        string
		maxBrightness    = baseline
	)
	attempts := 0
	snapshotErrors := 0

	for attempts+snapshotErrors < lightTestMaxFrames {
		if wait := time.Until(next); wait > 0 {
			if !lightTestSleep(ctx, wait) {
				s.finishLightTestCanceled(run, caller, entity, initialOn, opts.LeaveLightOn)
				return
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		data, snapshotMs, err := s.lightTestSnapshot(ctx)
		afterMs := time.Since(turnOnStart).Milliseconds()
		if err != nil {
			if ctx.Err() != nil {
				s.finishLightTestCanceled(run, caller, entity, initialOn, opts.LeaveLightOn)
				return
			}
			snapshotErrors++
			run.update(func(res *LightTestResult) { res.SnapshotErrors = snapshotErrors })
			log.Printf("[lighttest] 第 %d 次抓图失败: %v", attempts+snapshotErrors, err)
			if snapshotErrors >= 3 && attempts == 0 {
				run.finish(LightTestFailed, fmt.Sprintf("连续抓图失败: %v", err))
				s.restoreLightTest(run, caller, entity, initialOn, opts.LeaveLightOn)
				return
			}
			next = lightTestNextAttempt(next, interval)
			if next.After(deadline) {
				break
			}
			continue
		}
		brightness, err := imageBrightness(data)
		if err != nil {
			run.finish(LightTestFailed, fmt.Sprintf("分析截图失败: %v", err))
			s.restoreLightTest(run, caller, entity, initialOn, opts.LeaveLightOn)
			return
		}
		attempts++
		bright := brightness >= threshold
		if brightness > maxBrightness {
			maxBrightness = brightness
		}
		frame, saveErr := s.saveLightTestFrame(run, data, LightTestFrame{
			AfterMs:    afterMs,
			Brightness: brightness,
			SnapshotMs: snapshotMs,
			Bright:     bright,
		})
		if saveErr != nil {
			log.Printf("[lighttest] 保存截图失败: %v", saveErr)
		}
		run.appendFrame(frame)
		lastImage = frame.ImageUrl
		if firstAfterBright == nil {
			first := bright
			firstAfterBright = &first
		}
		run.update(func(res *LightTestResult) {
			res.Attempts = attempts
			res.MaxBrightness = maxBrightness
		})
		if bright {
			detected := afterMs
			detectMs = &detected
			brightImage = frame.ImageUrl
			break
		}
		next = lightTestNextAttempt(next, interval)
		if next.After(deadline) {
			break
		}
	}

	run.update(func(res *LightTestResult) {
		res.Attempts = attempts
		res.MaxBrightness = maxBrightness
		res.DetectMs = detectMs
		res.DelaySufficient = firstAfterBright
		res.BrightImage = brightImage
		res.LastImage = lastImage
	})
	s.restoreLightTest(run, caller, entity, initialOn, opts.LeaveLightOn)
	run.update(func(res *LightTestResult) { res.Advice = lightTestAdvice(*res) })
	if detectMs != nil {
		run.finish(LightTestDone, fmt.Sprintf("开灯后 %d ms 截到变亮的画面", *detectMs))
		return
	}
	run.finish(LightTestDone, fmt.Sprintf("%.0f 秒内没有截到明显变亮的画面", float64(opts.TimeoutMs)/1000))
}

// finishLightTestCanceled 结束被取消的测试，并恢复照明状态。
func (s *Service) finishLightTestCanceled(run *lightTestRun, caller ServiceCaller, entity string, initialOn, leaveOn bool) {
	s.restoreLightTest(run, caller, entity, initialOn, leaveOn)
	run.update(func(res *LightTestResult) { res.Advice = lightTestAdvice(*res) })
	run.finish(LightTestCanceled, "测试已取消")
}

// restoreLightTest 把灯恢复到测试前的状态：原本亮着的恢复为亮，
// 原本关着的默认关回去（勾选「保持亮灯」则留在亮的状态）。
func (s *Service) restoreLightTest(run *lightTestRun, caller ServiceCaller, entity string, initialOn, leaveOn bool) {
	service := "turn_off"
	state := "off"
	if initialOn || leaveOn {
		service = "turn_on"
		state = "on"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := caller.CallService(ctx, "light", service, map[string]any{"entity_id": entity}); err != nil {
		log.Printf("[lighttest] 恢复照明状态失败: %v", err)
		run.update(func(res *LightTestResult) { res.RestoreError = err.Error() })
		return
	}
	run.update(func(res *LightTestResult) { res.RestoredState = state })
}

func (s *Service) lightTestSnapshot(ctx context.Context) ([]byte, int64, error) {
	start := time.Now()
	data, err := s.source.Snapshot(ctx)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return nil, elapsed, err
	}
	if len(data) == 0 {
		return nil, elapsed, errors.New("摄像头返回空快照")
	}
	return data, elapsed, nil
}

func (s *Service) saveLightTestFrame(run *lightTestRun, data []byte, frame LightTestFrame) (LightTestFrame, error) {
	index := len(run.snapshot().Frames) + 1
	name := fmt.Sprintf("frame-%03d.jpg", index)
	if err := os.MkdirAll(run.dir, 0o755); err != nil {
		return frame, err
	}
	thumb, err := thumbnailJPEG(data, lightTestThumbWidth, 78)
	if err != nil {
		return frame, err
	}
	if err := os.WriteFile(filepath.Join(run.dir, name), thumb, 0o644); err != nil {
		return frame, err
	}
	frame.Index = index
	frame.ImageUrl = "/api/test/light-capture/image/" + run.id + "/" + name
	return frame, nil
}

func (s *Service) waitLightState(ctx context.Context, entity string, wantOn bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		state, err := s.entities.EntityState(ctx, entity)
		if err == nil && lightIsOn(state) == wantOn {
			return true
		}
		if !lightTestSleep(ctx, 250*time.Millisecond) {
			return false
		}
	}
	return false
}

func (s *Service) pruneLightTests(keep int) {
	root := filepath.Join(s.dataDir, "light-tests")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	if len(names) <= keep {
		return
	}
	// runID 是时间戳，字典序即时间序，保留最新的 keep 个。
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	s.lightTestMu.Lock()
	defer s.lightTestMu.Unlock()
	for _, name := range names[keep:] {
		if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
			log.Printf("[lighttest] 清理旧测试截图失败: %v", err)
			continue
		}
		delete(s.lightTestDirs, name)
	}
}

func lightTestSleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// lightTestNextAttempt 计算下一次抓图时间；抓图本身很慢时不做补偿式连拍。
func lightTestNextAttempt(next time.Time, interval time.Duration) time.Time {
	candidate := next.Add(interval)
	if now := time.Now(); candidate.Before(now) {
		candidate = now.Add(interval)
	}
	return candidate
}

// lightTestThreshold 由暗底亮度推出「已经变亮」的门槛：
// 暗底越亮，摄像头噪声和环境光带来的浮动越大，门槛也相应抬高。
func lightTestThreshold(baseline float64) float64 {
	delta := baseline * lightTestDeltaRatio
	if delta < lightTestMinDelta {
		delta = lightTestMinDelta
	}
	if delta > lightTestMaxDelta {
		delta = lightTestMaxDelta
	}
	return baseline + delta
}

func lightTestAdvice(res LightTestResult) string {
	if res.DetectMs == nil {
		return fmt.Sprintf("在 %.1f 秒内没有截到明显变亮的画面（暗底亮度 %.1f，最高 %.1f，判定门槛 %.1f）。请确认灯是否真的点亮、摄像头画面是否有更新延迟，或者环境光本来就已经很亮。",
			float64(res.TimeoutMs)/1000, res.BaselineBrightness, res.MaxBrightness, res.Threshold)
	}
	detect := *res.DetectMs
	if res.DelaySufficient != nil && *res.DelaySufficient {
		return fmt.Sprintf("设定延迟 %d ms 足够：按这个延迟抓到的第一张图就已经变亮（实测开灯到变亮约 %d ms）。为留余量，建议保持在 %d ms 以上。",
			res.DelayMs, detect, detect)
	}
	recommend := int(math.Ceil(float64(detect)*1.3/100)) * 100
	if recommend < 500 {
		recommend = 500
	}
	return fmt.Sprintf("设定延迟 %d ms 不够：开灯后约 %d ms 才截到变亮的画面，按设定延迟抓到的第一张图仍然偏暗。建议把「抓图前提前亮灯秒数」设为 %d ms（约 %.1f 秒）以上。",
		res.DelayMs, detect, recommend, float64(recommend)/1000)
}

// imageBrightness 返回整张图 0-100 的平均亮度（Rec.601），用来判断画面是否已经变亮。
func imageBrightness(data []byte) (float64, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return 0, fmt.Errorf("解析图片失败: %w", err)
	}
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return 0, errors.New("图片尺寸为空")
	}
	stepX, stepY := 1, 1
	if width > 240 {
		stepX = width / 240
	}
	if height > 240 {
		stepY = height / 240
	}
	var sum float64
	var count int
	for y := bounds.Min.Y; y < bounds.Max.Y; y += stepY {
		for x := bounds.Min.X; x < bounds.Max.X; x += stepX {
			r, g, b, _ := img.At(x, y).RGBA()
			// RGBA() 是 16 位，右移 8 位得到 0-255。
			sum += 0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(b>>8)
			count++
		}
	}
	if count == 0 {
		return 0, errors.New("图片没有可用像素")
	}
	return sum / float64(count) / 255 * 100, nil
}

// thumbnailJPEG 把快照缩到指定宽度再存盘，避免测试页留下大量原图。
func thumbnailJPEG(data []byte, maxWidth, quality int) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("解析图片失败: %w", err)
	}
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return nil, errors.New("图片尺寸为空")
	}
	outWidth, outHeight := width, height
	if maxWidth > 0 && width > maxWidth {
		outWidth = maxWidth
		outHeight = int(float64(height) * float64(maxWidth) / float64(width))
		if outHeight < 1 {
			outHeight = 1
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, outWidth, outHeight))
	for y := 0; y < outHeight; y++ {
		srcY := bounds.Min.Y + y*height/outHeight
		for x := 0; x < outWidth; x++ {
			srcX := bounds.Min.X + x*width/outWidth
			dst.Set(x, y, img.At(srcX, srcY))
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("压缩截图失败: %w", err)
	}
	return buf.Bytes(), nil
}
