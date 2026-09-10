package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/zhf883680/LayerWatch/internal/app"
	"github.com/zhf883680/LayerWatch/internal/config"
	"github.com/zhf883680/LayerWatch/internal/ffmpeg"
	"github.com/zhf883680/LayerWatch/internal/ha"
	"github.com/zhf883680/LayerWatch/internal/httpapi"
	"github.com/zhf883680/LayerWatch/internal/notify"
	"github.com/zhf883680/LayerWatch/internal/store"
	"github.com/zhf883680/LayerWatch/internal/vision"
)

func main() {
	configPath := flag.String("config", envOr("CONFIG_PATH", "config/config.yaml"), "配置文件路径")
	dataDir := flag.String("data", envOr("DATA_DIR", "data"), "数据目录")
	flag.Parse()

	cfg, err := config.Open(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if _, err := os.Stat(*configPath); os.IsNotExist(err) {
		if err := cfg.Save(); err != nil {
			log.Printf("保存默认配置失败: %v", err)
		}
	}
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}
	db, err := store.Open(filepath.Join(*dataDir, "monitor.db"))
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()

	haClient := ha.New(cfg)
	notifier := notify.New(cfg, haClient)
	application := app.New(
		cfg,
		db,
		*dataDir,
		haClient,
		haClient,
		notifier,
		vision.New(cfg),
		ffmpeg.NewEncoder(),
	)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	application.Run(ctx)

	addr := envOr("ADDR", ":19091")
	server := &http.Server{
		Addr:              addr,
		Handler:           httpapi.New(cfg, application).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Println("================================")
	log.Println("  LayerWatch · 层哨")
	log.Println("================================")
	log.Printf("  Web       : http://%s", displayAddr(addr))
	log.Printf("  配置      : %s", *configPath)
	log.Printf("  数据      : %s", *dataDir)
	log.Printf("  FFmpeg    : %s", application.Status()["ffmpeg"])
	log.Println("================================")
	if err := cfg.Get().Validate(); err != nil {
		log.Printf("配置尚未完整: %v（可在网页中填写）", err)
	}

	errCh := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	select {
	case <-ctx.Done():
		log.Println("正在关闭服务...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	case err := <-errCh:
		log.Fatalf("HTTP 服务失败: %v", err)
	}
}

func displayAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "0.0.0.0" + addr
	}
	return addr
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
