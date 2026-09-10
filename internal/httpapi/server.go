package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zhf883680/LayerWatch/internal/app"
	"github.com/zhf883680/LayerWatch/internal/config"
	"github.com/zhf883680/LayerWatch/web"
)

type Server struct {
	cfg *config.Store
	app *app.Service
}

func New(cfg *config.Store, application *app.Service) *Server {
	return &Server{cfg: cfg, app: application}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/config", s.getConfig)
	mux.HandleFunc("PUT /api/config", s.updateConfig)
	mux.HandleFunc("POST /api/test/ha", s.testHA)
	mux.HandleFunc("POST /api/test/ai", s.testAI)
	mux.HandleFunc("POST /api/test/notification", s.testNotification)
	mux.HandleFunc("POST /api/test/light", s.testLight)

	mux.HandleFunc("POST /api/session/start", s.start)
	mux.HandleFunc("POST /api/session/stop", s.stop)
	mux.HandleFunc("POST /api/session/layer", s.layer)
	mux.HandleFunc("POST /api/session/capture", s.capture)
	mux.HandleFunc("GET /api/sessions", s.sessions)

	// 兼容旧 LapseCam 的 HA rest_command，迁移 HA 自动化时无需改 URL。
	mux.HandleFunc("POST /api/quick/start", s.start)
	mux.HandleFunc("POST /api/quick/stop", s.stop)
	mux.HandleFunc("POST /api/quick/snapshot", s.layer)
	mux.HandleFunc("POST /api/quick/layer", s.layer)
	mux.HandleFunc("GET /api/quick/check", s.latestCheck)

	mux.HandleFunc("GET /api/checks", s.checks)
	mux.HandleFunc("GET /api/checks/{id}/image", s.checkImage)
	mux.HandleFunc("GET /api/videos", s.videos)
	mux.HandleFunc("GET /api/videos/{id}/file", s.videoFile)
	mux.HandleFunc("DELETE /api/videos/{id}", s.deleteVideo)
	mux.HandleFunc("POST /api/cleanup", s.cleanup)

	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(web.Assets))))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, web.IndexHTML)
	})
	return s.middleware(mux)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("deep") == "1" {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := s.app.CheckHA(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "error", "message": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	payload := s.app.Status()
	payload["config"] = s.cfg.Get()
	if latest, _ := s.app.LatestCheck(0); latest != nil {
		payload["latestGlobalCheck"] = latest
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) getConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.cfg.Get())
}

func (s *Server) updateConfig(w http.ResponseWriter, r *http.Request) {
	var cfg config.Config
	if err := decodeJSON(r, &cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.cfg.Update(cfg); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.cfg.Get())
}

func (s *Server) testHA(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.app.TestHA(ctx))
}

func (s *Server) testAI(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	writeJSON(w, http.StatusOK, s.app.TestAI(ctx))
}

func (s *Server) testNotification(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.app.TestNotification(ctx))
}

func (s *Server) testLight(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, s.app.TestLight(ctx))
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	session, already, err := s.app.Start(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	message := "已开始打印任务"
	if already {
		message = "任务已在运行"
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": session, "message": message})
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	session, found, err := s.app.Stop(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		writeJSON(w, http.StatusOK, map[string]any{"message": "当前没有进行中的打印任务"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": session, "message": "已停止抓图，正在生成延时视频"})
}

func (s *Server) layer(w http.ResponseWriter, r *http.Request) {
	layer, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("layer")))
	if err != nil || layer <= 0 {
		writeError(w, http.StatusBadRequest, "layer 必须是正整数")
		return
	}
	frame, captured, err := s.app.CaptureLayer(r.Context(), layer)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	message := "已抓取该层"
	if !captured {
		message = "当前触发模式无需抓图，或该层已抓取"
	}
	writeJSON(w, http.StatusOK, map[string]any{"frame": frame, "captured": captured, "message": message})
}

func (s *Server) capture(w http.ResponseWriter, r *http.Request) {
	frame, captured, err := s.app.CaptureNow(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"frame": frame, "captured": captured})
}

func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := s.app.ListSessions(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) checks(w http.ResponseWriter, r *http.Request) {
	sessionID, _ := strconv.ParseInt(r.URL.Query().Get("sessionId"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := s.app.ListChecks(sessionID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) latestCheck(w http.ResponseWriter, r *http.Request) {
	sessionID, _ := strconv.ParseInt(r.URL.Query().Get("sessionId"), 10, 64)
	if sessionID == 0 {
		if active, _ := s.app.ActiveSession(); active != nil {
			sessionID = active.ID
		} else if latest, _ := s.app.LatestSession(); latest != nil {
			sessionID = latest.ID
		}
	}
	if sessionID == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"found": false})
		return
	}
	check, err := s.app.LatestCheck(sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if check == nil {
		writeJSON(w, http.StatusOK, map[string]any{"found": false, "sessionId": sessionID})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"found": true, "check": check})
}

func (s *Server) checkImage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "无效 ID")
		return
	}
	path, err := s.app.CheckImagePath(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	http.ServeFile(w, r, path)
}

func (s *Server) videos(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := s.app.ListVideos(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) videoFile(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "无效 ID")
		return
	}
	video, err := s.app.Video(id)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if _, err := os.Stat(video.FilePath); err != nil {
		writeError(w, http.StatusNotFound, "视频文件不存在")
		return
	}
	w.Header().Set("Content-Type", videoContentType(video.FilePath))
	w.Header().Set("Content-Disposition", `inline; filename="`+strings.ReplaceAll(video.FileName, `"`, "")+`"`)
	http.ServeFile(w, r, video.FilePath)
}

// videoContentType 显式给出视频 MIME：容器里可能没有 /etc/mime.types，
// 那样 http.ServeFile 会把 mp4 嗅探成 application/octet-stream，浏览器只会下载。
func videoContentType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	case ".webm":
		return "video/webm"
	case ".mkv":
		return "video/x-matroska"
	default:
		return "application/octet-stream"
	}
}

func (s *Server) deleteVideo(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "无效 ID")
		return
	}
	if err := s.app.DeleteVideo(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) cleanup(w http.ResponseWriter, r *http.Request) {
	stats, err := s.app.Cleanup(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	return dec.Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
