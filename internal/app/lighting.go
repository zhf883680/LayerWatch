package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

type ServiceCaller interface {
	CallService(ctx context.Context, domain, service string, data any) error
}

func (s *Service) prepareCaptureLight(ctx context.Context, sessionID int64) func() {
	noop := func() {}
	cfg := s.cfg.Get().Lighting
	if !cfg.Enabled || strings.TrimSpace(cfg.Entity) == "" {
		return noop
	}
	caller, ok := s.source.(ServiceCaller)
	if !ok {
		log.Printf("[light] 当前 HA 客户端不支持服务调用")
		return noop
	}
	state, err := s.entities.EntityState(ctx, cfg.Entity)
	if err != nil {
		log.Printf("[light] 读取照明实体失败: %v", err)
		return noop
	}
	// 灯原本已经打开时不改变用户状态，也不在抓图后关闭。
	if lightIsOn(state) {
		return noop
	}
	if !lightIsOff(state) {
		log.Printf("[light] 照明实体状态为 %q，跳过自动控制", state)
		return noop
	}
	if err := caller.CallService(ctx, "light", "turn_on", map[string]any{"entity_id": cfg.Entity}); err != nil {
		log.Printf("[light] 开灯失败: %v", err)
		return noop
	}
	turnOff := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := caller.CallService(ctx, "light", "turn_off", map[string]any{"entity_id": cfg.Entity}); err != nil {
			log.Printf("[light] 关灯失败: %v", err)
		}
	}
	if cfg.DelaySeconds > 0 {
		select {
		case <-ctx.Done():
			return turnOff
		case <-time.After(time.Duration(cfg.DelaySeconds) * time.Second):
		}
	}
	if cfg.KeepOnDuringPrint {
		s.setSessionLight(sessionID, true)
		return noop
	}
	return turnOff
}

func (s *Service) releaseSessionLight(sessionID int64) {
	if !s.sessionLightOn(sessionID) {
		return
	}
	cfg := s.cfg.Get().Lighting
	caller, ok := s.source.(ServiceCaller)
	if !ok || strings.TrimSpace(cfg.Entity) == "" {
		s.setSessionLight(sessionID, false)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := caller.CallService(ctx, "light", "turn_off", map[string]any{"entity_id": cfg.Entity}); err != nil {
		log.Printf("[light] 停止任务后关灯失败: %v", err)
	}
	s.setSessionLight(sessionID, false)
}

func (s *Service) setSessionLight(sessionID int64, on bool) {
	s.lightMu.Lock()
	defer s.lightMu.Unlock()
	if on {
		s.lightSessions[sessionID] = true
	} else {
		delete(s.lightSessions, sessionID)
	}
}

func (s *Service) sessionLightOn(sessionID int64) bool {
	s.lightMu.Lock()
	defer s.lightMu.Unlock()
	return s.lightSessions[sessionID]
}

func (s *Service) TestLight(ctx context.Context) DiagnosticResult {
	result := DiagnosticResult{OK: true}
	cfg := s.cfg.Get().Lighting
	add := func(err error, message string) {
		check := DiagnosticCheck{Name: "照明实体", OK: err == nil}
		if err != nil {
			check.Message = err.Error()
			result.OK = false
		} else {
			check.Message = message
		}
		result.Checks = append(result.Checks, check)
	}
	if !cfg.Enabled {
		add(errors.New("自动照明未启用"), "")
		return result
	}
	if strings.TrimSpace(cfg.Entity) == "" {
		add(errors.New("未配置照明实体"), "")
		return result
	}
	caller, ok := s.source.(ServiceCaller)
	if !ok {
		add(errors.New("当前 HA 客户端不支持服务调用"), "")
		return result
	}
	state, err := s.entities.EntityState(ctx, cfg.Entity)
	if err != nil {
		add(err, "")
		return result
	}
	initialOn := lightIsOn(state)
	if !initialOn && !lightIsOff(state) {
		add(fmt.Errorf("实体状态为 %q，无法确认灯具状态", state), "")
		return result
	}
	if !initialOn {
		if err := caller.CallService(ctx, "light", "turn_on", map[string]any{"entity_id": cfg.Entity}); err != nil {
			add(err, "")
			return result
		}
		select {
		case <-ctx.Done():
			add(ctx.Err(), "")
			return result
		case <-time.After(time.Second):
		}
		after, err := s.entities.EntityState(ctx, cfg.Entity)
		if err != nil {
			add(err, "")
			return result
		}
		restoreCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = caller.CallService(restoreCtx, "light", "turn_off", map[string]any{"entity_id": cfg.Entity})
		cancel()
		if !lightIsOn(after) {
			add(fmt.Errorf("调用开灯后状态仍为 %q", after), "")
			return result
		}
	}
	add(nil, fmt.Sprintf("%s 可正常控制，当前状态=%s", cfg.Entity, state))
	return result
}

func lightIsOn(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "on", "true", "1":
		return true
	default:
		return false
	}
}

func lightIsOff(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "off", "false", "0":
		return true
	default:
		return false
	}
}
