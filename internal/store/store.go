package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	SessionRunning  = "running"
	SessionEncoding = "encoding"
	SessionComplete = "completed"
	SessionFailed   = "failed"
)

type Session struct {
	ID          int64      `json:"id"`
	StartedAt   time.Time  `json:"startedAt"`
	EndedAt     *time.Time `json:"endedAt,omitempty"`
	Status      string     `json:"status"`
	TriggerMode string     `json:"triggerMode"`
	FrameCount  int        `json:"frameCount"`
	LayerCount  int        `json:"layerCount"`
	AIChecks    int        `json:"aiChecks"`
	Error       string     `json:"error,omitempty"`
}

type Frame struct {
	ID        int64     `json:"id"`
	SessionID int64     `json:"sessionId"`
	FrameNo   int       `json:"frameNo"`
	Layer     int       `json:"layer"`
	Path      string    `json:"-"`
	CreatedAt time.Time `json:"createdAt"`
}

type Check struct {
	ID         int64     `json:"id"`
	SessionID  int64     `json:"sessionId"`
	Status     string    `json:"status"`
	Confidence float64   `json:"confidence"`
	Reason     string    `json:"reason"`
	ImagePath  string    `json:"-"`
	ImageURL   string    `json:"imageUrl,omitempty"`
	Alert      bool      `json:"alert"`
	CreatedAt  time.Time `json:"createdAt"`
}

type Video struct {
	ID              int64     `json:"id"`
	SessionID       int64     `json:"sessionId"`
	FilePath        string    `json:"-"`
	FileName        string    `json:"fileName"`
	FileSize        int64     `json:"fileSize"`
	FrameCount      int       `json:"frameCount"`
	DurationSeconds float64   `json:"durationSeconds"`
	Status          string    `json:"status"`
	Error           string    `json:"error,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据库目录: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开数据库: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("设置数据库参数: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS sessions (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  started_at    TEXT NOT NULL,
  ended_at      TEXT,
  status        TEXT NOT NULL,
  trigger_mode  TEXT NOT NULL,
  error_message TEXT NOT NULL DEFAULT '',
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status, id);

CREATE TABLE IF NOT EXISTS frames (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id INTEGER NOT NULL,
  frame_no   INTEGER NOT NULL,
  layer      INTEGER NOT NULL DEFAULT 0,
  path       TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE(session_id, frame_no)
);
CREATE INDEX IF NOT EXISTS idx_frames_session ON frames(session_id, frame_no);

CREATE TABLE IF NOT EXISTS checks (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id INTEGER NOT NULL,
  status     TEXT NOT NULL,
  confidence REAL NOT NULL DEFAULT 0,
  reason     TEXT NOT NULL DEFAULT '',
  image_path TEXT NOT NULL DEFAULT '',
  alert      INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_checks_session ON checks(session_id, id);

CREATE TABLE IF NOT EXISTS videos (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id       INTEGER NOT NULL,
  file_path        TEXT NOT NULL,
  file_size        INTEGER NOT NULL DEFAULT 0,
  frame_count      INTEGER NOT NULL DEFAULT 0,
  duration_seconds REAL NOT NULL DEFAULT 0,
  status           TEXT NOT NULL,
  error_message    TEXT NOT NULL DEFAULT '',
  created_at       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_videos_session ON videos(session_id, id);
`)
	if err != nil {
		return fmt.Errorf("初始化数据库: %w", err)
	}
	// 进程重启后，内存中的抽帧/编码任务已不存在。
	_, _ = db.Exec(`UPDATE sessions SET status=?, error_message=?, ended_at=?, updated_at=? WHERE status IN (?, ?)`,
		SessionFailed, "服务重启，任务已中断", now(), now(), SessionRunning, SessionEncoding)
	return nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func parseTime(v string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, v)
	return t
}

func (s *Store) CreateSession(triggerMode string) (Session, error) {
	started := now()
	res, err := s.db.Exec(`INSERT INTO sessions(started_at,status,trigger_mode,created_at,updated_at) VALUES(?,?,?,?,?)`,
		started, SessionRunning, triggerMode, started, started)
	if err != nil {
		return Session{}, err
	}
	id, _ := res.LastInsertId()
	return s.GetSession(id)
}

func (s *Store) ActiveSession() (*Session, error) {
	session, err := scanSession(s.db.QueryRow(sessionSelect()+` WHERE status=? ORDER BY id DESC LIMIT 1`, SessionRunning))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &session, nil
}

func (s *Store) LatestSession() (*Session, error) {
	session, err := scanSession(s.db.QueryRow(sessionSelect() + ` ORDER BY id DESC LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &session, nil
}

func (s *Store) GetSession(id int64) (Session, error) {
	session, err := scanSession(s.db.QueryRow(sessionSelect()+` WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, errors.New("任务不存在")
	}
	return session, err
}

func (s *Store) ListSessions(limit int) ([]Session, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(sessionSelect()+` ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Session, 0)
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, session)
	}
	return out, rows.Err()
}

func (s *Store) SetSessionStatus(id int64, status, message string) error {
	ended := any(nil)
	if status == SessionComplete || status == SessionFailed {
		ended = now()
	}
	_, err := s.db.Exec(`UPDATE sessions SET status=?, error_message=?, ended_at=COALESCE(?, ended_at), updated_at=? WHERE id=?`,
		status, strings.TrimSpace(message), ended, now(), id)
	return err
}

func (s *Store) CountFrames(sessionID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM frames WHERE session_id=?`, sessionID).Scan(&n)
	return n, err
}

func (s *Store) AddFrame(sessionID int64, frameNo, layer int, path string) (Frame, error) {
	created := now()
	_, err := s.db.Exec(`INSERT INTO frames(session_id,frame_no,layer,path,created_at) VALUES(?,?,?,?,?)`,
		sessionID, frameNo, layer, path, created)
	if err != nil {
		return Frame{}, err
	}
	var id int64
	_ = s.db.QueryRow(`SELECT id FROM frames WHERE session_id=? AND frame_no=?`, sessionID, frameNo).Scan(&id)
	return Frame{ID: id, SessionID: sessionID, FrameNo: frameNo, Layer: layer, Path: path, CreatedAt: parseTime(created)}, nil
}

func (s *Store) ListFrames(sessionID int64) ([]Frame, error) {
	rows, err := s.db.Query(`SELECT id,session_id,frame_no,layer,path,created_at FROM frames WHERE session_id=? ORDER BY frame_no`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Frame, 0)
	for rows.Next() {
		var frame Frame
		var created string
		if err := rows.Scan(&frame.ID, &frame.SessionID, &frame.FrameNo, &frame.Layer, &frame.Path, &created); err != nil {
			return nil, err
		}
		frame.CreatedAt = parseTime(created)
		out = append(out, frame)
	}
	return out, rows.Err()
}

func (s *Store) HasLayer(sessionID int64, layer int) (bool, error) {
	if layer <= 0 {
		return false, nil
	}
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM frames WHERE session_id=? AND layer=?`, sessionID, layer).Scan(&n)
	return n > 0, err
}

func (s *Store) CountChecks(sessionID int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM checks WHERE session_id=?`, sessionID).Scan(&n)
	return n, err
}

func (s *Store) AddCheck(sessionID int64, status string, confidence float64, reason, imagePath string, alert bool) (Check, error) {
	created := now()
	alertInt := 0
	if alert {
		alertInt = 1
	}
	res, err := s.db.Exec(`INSERT INTO checks(session_id,status,confidence,reason,image_path,alert,created_at) VALUES(?,?,?,?,?,?,?)`,
		sessionID, status, confidence, reason, imagePath, alertInt, created)
	if err != nil {
		return Check{}, err
	}
	id, _ := res.LastInsertId()
	return Check{ID: id, SessionID: sessionID, Status: status, Confidence: confidence, Reason: reason,
		ImagePath: imagePath, Alert: alert, CreatedAt: parseTime(created)}, nil
}

func (s *Store) LatestCheck(sessionID int64) (*Check, error) {
	check, err := scanCheck(s.db.QueryRow(`SELECT id,session_id,status,confidence,reason,image_path,alert,created_at FROM checks WHERE session_id=? ORDER BY id DESC LIMIT 1`, sessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &check, nil
}

func (s *Store) ListChecks(sessionID int64, limit int) ([]Check, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT id,session_id,status,confidence,reason,image_path,alert,created_at FROM checks`
	args := []any{}
	if sessionID > 0 {
		query += ` WHERE session_id=?`
		args = append(args, sessionID)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Check, 0)
	for rows.Next() {
		check, err := scanCheck(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, check)
	}
	return out, rows.Err()
}

func (s *Store) LastAlertAt(sessionID int64) (time.Time, error) {
	var created sql.NullString
	err := s.db.QueryRow(`SELECT MAX(created_at) FROM checks WHERE session_id=? AND alert=1`, sessionID).Scan(&created)
	if err != nil || !created.Valid {
		return time.Time{}, err
	}
	return parseTime(created.String), nil
}

func (s *Store) RecentChecks(sessionID int64, limit int) ([]Check, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT id,session_id,status,confidence,reason,image_path,alert,created_at
		FROM checks WHERE session_id=? ORDER BY id DESC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Check, 0)
	for rows.Next() {
		check, err := scanCheck(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, check)
	}
	return out, rows.Err()
}

func (s *Store) AddVideo(sessionID int64, path string, size int64, frames int, fps int, status, message string) (Video, error) {
	created := now()
	duration := 0.0
	if fps > 0 {
		duration = float64(frames) / float64(fps)
	}
	res, err := s.db.Exec(`INSERT INTO videos(session_id,file_path,file_size,frame_count,duration_seconds,status,error_message,created_at)
		VALUES(?,?,?,?,?,?,?,?)`, sessionID, path, size, frames, duration, status, message, created)
	if err != nil {
		return Video{}, err
	}
	id, _ := res.LastInsertId()
	return Video{ID: id, SessionID: sessionID, FilePath: path, FileName: filepath.Base(path), FileSize: size,
		FrameCount: frames, DurationSeconds: duration, Status: status, Error: message, CreatedAt: parseTime(created)}, nil
}

func (s *Store) ListVideos(limit int) ([]Video, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id,session_id,file_path,file_size,frame_count,duration_seconds,status,error_message,created_at
		FROM videos ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Video, 0)
	for rows.Next() {
		video, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, video)
	}
	return out, rows.Err()
}

func (s *Store) GetVideo(id int64) (Video, error) {
	video, err := scanVideo(s.db.QueryRow(`SELECT id,session_id,file_path,file_size,frame_count,duration_seconds,status,error_message,created_at
		FROM videos WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Video{}, errors.New("视频不存在")
	}
	return video, err
}

func (s *Store) DeleteVideo(id int64) error {
	res, err := s.db.Exec(`DELETE FROM videos WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("视频不存在")
	}
	return nil
}

func (s *Store) DeleteChecksBefore(cutoff time.Time) ([]string, error) {
	rows, err := s.db.Query(`SELECT image_path FROM checks WHERE created_at < ? AND image_path <> ''`, cutoff.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err == nil && path != "" {
			paths = append(paths, path)
		}
	}
	rows.Close()
	if _, err := s.db.Exec(`DELETE FROM checks WHERE created_at < ?`, cutoff.UTC().Format(time.RFC3339Nano)); err != nil {
		return nil, err
	}
	return paths, nil
}

func sessionSelect() string {
	return `SELECT s.id,s.started_at,s.ended_at,s.status,s.trigger_mode,s.error_message,
		(SELECT COUNT(*) FROM frames f WHERE f.session_id=s.id),
		(SELECT COUNT(DISTINCT layer) FROM frames f WHERE f.session_id=s.id AND f.layer>0),
		(SELECT COUNT(*) FROM checks c WHERE c.session_id=s.id)
		FROM sessions s`
}

type rowScanner interface{ Scan(...any) error }

func scanSession(r rowScanner) (Session, error) {
	var session Session
	var ended sql.NullString
	var started string
	if err := r.Scan(&session.ID, &started, &ended, &session.Status, &session.TriggerMode, &session.Error,
		&session.FrameCount, &session.LayerCount, &session.AIChecks); err != nil {
		return Session{}, err
	}
	session.StartedAt = parseTime(started)
	if ended.Valid && ended.String != "" {
		t := parseTime(ended.String)
		session.EndedAt = &t
	}
	return session, nil
}

func scanCheck(r rowScanner) (Check, error) {
	var check Check
	var alert int
	var created string
	if err := r.Scan(&check.ID, &check.SessionID, &check.Status, &check.Confidence, &check.Reason, &check.ImagePath, &alert, &created); err != nil {
		return Check{}, err
	}
	check.Alert = alert == 1
	check.CreatedAt = parseTime(created)
	if check.ImagePath != "" {
		check.ImageURL = fmt.Sprintf("/api/checks/%d/image", check.ID)
	}
	return check, nil
}

func scanVideo(r rowScanner) (Video, error) {
	var video Video
	var created string
	if err := r.Scan(&video.ID, &video.SessionID, &video.FilePath, &video.FileSize, &video.FrameCount,
		&video.DurationSeconds, &video.Status, &video.Error, &created); err != nil {
		return Video{}, err
	}
	video.FileName = filepath.Base(video.FilePath)
	video.CreatedAt = parseTime(created)
	return video, nil
}

func (s *Store) GetCheck(id int64) (Check, error) {
	check, err := scanCheck(s.db.QueryRow(`SELECT id,session_id,status,confidence,reason,image_path,alert,created_at FROM checks WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Check{}, errors.New("分析记录不存在")
	}
	return check, err
}
