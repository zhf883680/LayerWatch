package ffmpeg

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

const (
	defaultBinary  = "ffmpeg"
	defaultFPS     = 30
	defaultPreset  = "veryfast"
	defaultCRF     = 27
	defaultMaxRate = 4000
)

type Encoder struct {
	binary string
}

func NewEncoder() *Encoder {
	binary := os.Getenv("FFMPEG_BINARY")
	if binary == "" {
		binary = defaultBinary
	}
	return &Encoder{binary: binary}
}

func (e *Encoder) Binary() string { return e.binary }

// Encode 把按文件名顺序排列的 JPEG 合成 MP4。编码参数固定为 ARM 友好的 x264 配置。
func (e *Encoder) Encode(ctx context.Context, framesDir string, outputPath string) error {
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return err
	}
	args := []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-framerate", strconv.Itoa(defaultFPS),
		"-i", filepath.Join(framesDir, "%06d.jpg"),
		"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2",
		"-c:v", "libx264",
		"-preset", defaultPreset,
		"-crf", strconv.Itoa(defaultCRF),
		"-maxrate", fmt.Sprintf("%dk", defaultMaxRate),
		"-bufsize", fmt.Sprintf("%dk", defaultMaxRate*2),
		"-pix_fmt", "yuv420p",
		"-movflags", "+faststart",
		outputPath,
	}
	cmd := exec.CommandContext(ctx, e.binary, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		if len(out) > 2048 {
			out = out[len(out)-2048:]
		}
		return fmt.Errorf("ffmpeg 编码失败: %w: %s", err, string(out))
	}
	return nil
}

func FPS() int { return defaultFPS }
