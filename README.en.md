<p align="center">
  <img src="web/layerwatch-512.png" width="150" alt="LayerWatch">
</p>

<h1 align="center">LayerWatch</h1>

<p align="center">
  <a href="https://github.com/zhf883680/LayerWatch">https://github.com/zhf883680/LayerWatch</a><br>
  <a href="README.md">简体中文</a> | <a href="README.en.md">English</a>
</p>

LayerWatch is a print monitoring service for Bambu Lab A1 printers that depends on Home Assistant:

- Captures images from a Home Assistant camera entity. No direct RTSP management.
- AI anomaly detection for spaghetti, clogging/material buildup, shifted objects, and nozzle collisions.
- Timelapse video generation using the same captured frames and automatic MP4 encoding after printing.
- Triggering by layer changes or a fixed time interval. Entity IDs are explicitly configured with no auto-discovery.
- Notifications through Home Assistant persistent notifications and optionally Bark. No webhook or image host required.
- Automatic cleanup of videos and alert images by retention period, with intermediate frames removed after encoding.
- Built-in defaults for FFmpeg, database, storage, and encoding settings.
- Bilingual web interface with Chinese and English language switching.

## Screenshots

### AI Analysis

![AI Analysis](img/ai-analysis.png)

### Timelapse

![Timelapse](img/timelapse.png)

### Settings

![Settings](img/settings.png)

## Configuration

```yaml
homeAssistant:
  baseURL: "http://homeassistant.local:8123"
  token: "Home Assistant long-lived access token"
  cameraEntity: "camera.bambu_lab_a1"
  layerEntity: "sensor.bambu_lab_a1_current_layer"
  statusEntity: "sensor.bambu_lab_a1_print_status"

ai:
  enabled: true
  baseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1"
  apiKey: "Vision model API key"
  model: "qwen3-vl-flash"
  maxChecksPerPrint: 50

trigger:
  mode: "layer" # layer | interval
  intervalSeconds: 10 # interval mode only, minimum 2 seconds

cleanup:
  retentionDays: 7 # 0 = keep forever

notification:
  haEnabled: true # Home Assistant persistent notifications
  barkEnabled: false
  barkKey: ""
  barkBaseURL: "https://api.day.app"
  barkGroup: "LayerWatch"
  barkLevel: "" # active / timeSensitive / critical
  barkVolume: 0 # critical volume, 0-10
```

Settings can also be edited from the web interface. The Home Assistant token and AI API key are stored in the configuration file, so restrict access to that file.

## Run

```bash
git clone https://github.com/zhf883680/LayerWatch.git
cd LayerWatch
docker compose pull
docker compose up -d
```

Docker Hub image:

```text
zhf883680/layerwatch:latest
```

Build from source:

```bash
docker build -t layerwatch .
```

Automated publishing requires these GitHub Actions secrets:

```text
DOCKERHUB_USERNAME
DOCKERHUB_TOKEN
```

Open the web interface at:

```text
http://<server-ip>:19091
```

Run locally:

```bash
go run ./cmd/server
```

FFmpeg must be installed locally, or set `FFMPEG_BINARY` to the executable path.

## Trigger Rules

### By Layer (recommended for A1)

1. LayerWatch reads `homeAssistant.statusEntity` every 2 seconds.
2. A print task starts automatically when the state becomes `printing`.
3. It reads `homeAssistant.layerEntity` and captures one frame when the layer number increases.
4. It stops and starts encoding when the state becomes `idle`, `finished`, `failed`, `stopped`, or `offline`.
5. A paused task remains active and continues when new layers appear after resuming.

If the status entity is empty, the task starts on the first valid layer number. Use `POST /api/session/stop` to stop it manually.

### By Time Interval

The first frame is captured as soon as the print state becomes `printing`. Additional frames are captured every `intervalSeconds`. No frames are captured while paused, and encoding starts when the print finishes.

## Home Assistant Entities

Typical entities exposed by the official Bambu Lab Home Assistant integration:

- Camera: `camera.<printer>_camera`
- Current layer: `sensor.<printer>_current_layer`
- Print status: `sensor.<printer>_print_status` or `sensor.<printer>_status`

Use Home Assistant Developer Tools → States to confirm the actual entity IDs. LayerWatch never guesses paths or entities.

## API

```text
GET    /api/status
GET    /api/config
PUT    /api/config
POST   /api/test/ha
POST   /api/test/ai
POST   /api/test/notification
POST   /api/session/start
POST   /api/session/stop
POST   /api/session/layer?layer=N
POST   /api/session/capture
GET    /api/sessions
GET    /api/checks
GET    /api/checks/{id}/image
GET    /api/videos
GET    /api/videos/{id}/file
DELETE /api/videos/{id}
POST   /api/cleanup
GET    /api/health?deep=1
```

The legacy LapseCam endpoints `/api/quick/start`, `/api/quick/stop`, `/api/quick/snapshot?layer=N`, and `/api/quick/check` are also compatible.

## AI Detection and Alerts

After each capture, LayerWatch analyzes the five most recent frames together. A notification is sent to every enabled HA/Bark channel when:

- AI classifies the result as abnormal;
- confidence is at least 0.8;
- three consecutive checks are abnormal;
- at least five minutes have passed since the previous alert.

Each print task can make at most `maxChecksPerPrint` AI calls. Set it to `0` for unlimited calls.

## Data

The default data directory is `./data`:

```text
data/
  monitor.db
  frames/session-<id>/
  videos/
  events/session-<id>/
```

Cleanup runs every six hours and removes videos and alert images according to `cleanup.retentionDays`. Intermediate frames are deleted immediately after successful encoding.
