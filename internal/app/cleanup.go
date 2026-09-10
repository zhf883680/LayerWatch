package app

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zhf883680/LayerWatch/internal/store"
)

type CleanupStats struct {
	DeletedVideos int   `json:"deletedVideos"`
	DeletedChecks int   `json:"deletedChecks"`
	DeletedFrames int   `json:"deletedFrames"`
	FreedBytes    int64 `json:"freedBytes"`
}

func (s *Service) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stats, err := s.Cleanup(ctx)
			if err != nil {
				log.Printf("[cleanup] 自动清理失败: %v", err)
				continue
			}
			log.Printf("[cleanup] 完成: 视频=%d 分析=%d 帧目录=%d 释放=%d bytes",
				stats.DeletedVideos, stats.DeletedChecks, stats.DeletedFrames, stats.FreedBytes)
		}
	}
}

func (s *Service) Cleanup(ctx context.Context) (CleanupStats, error) {
	var stats CleanupStats
	cfg := s.cfg.Get()
	if cfg.Cleanup.RetentionDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -cfg.Cleanup.RetentionDays)
		videos, err := s.db.ListVideos(500)
		if err != nil {
			return stats, err
		}
		for _, video := range videos {
			if !video.CreatedAt.Before(cutoff) {
				continue
			}
			if info, err := os.Stat(video.FilePath); err == nil {
				stats.FreedBytes += info.Size()
			}
			if err := s.db.DeleteVideo(video.ID); err != nil && !strings.Contains(err.Error(), "不存在") {
				return stats, err
			}
			_ = os.Remove(video.FilePath)
			stats.DeletedVideos++
		}
		paths, err := s.db.DeleteChecksBefore(cutoff)
		if err != nil {
			return stats, err
		}
		for _, path := range paths {
			_ = os.Remove(path)
			stats.DeletedChecks++
		}
	}

	framesRoot := filepath.Join(s.dataDir, "frames")
	entries, err := os.ReadDir(framesRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return stats, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "session-") {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(entry.Name(), "session-"), 10, 64)
		if err != nil {
			continue
		}
		if s.isActive(id) {
			continue
		}
		session, err := s.db.GetSession(id)
		if err == nil && session.Status == store.SessionRunning {
			continue
		}
		path := filepath.Join(framesRoot, entry.Name())
		size := dirSize(path)
		if err := os.RemoveAll(path); err == nil {
			stats.DeletedFrames++
			stats.FreedBytes += size
		}
	}
	_ = filepath.WalkDir(filepath.Join(s.dataDir, "events"), func(path string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() && path != filepath.Join(s.dataDir, "events") {
			_ = os.Remove(path)
		}
		return nil
	})
	return stats, nil
}

func dirSize(root string) int64 {
	var size int64
	_ = filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if info, err := entry.Info(); err == nil {
			size += info.Size()
		}
		return nil
	})
	return size
}
