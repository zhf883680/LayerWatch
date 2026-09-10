<p align="center">
  <img src="web/layerwatch-512.png" width="150" alt="LayerWatch">
</p>

<h1 align="center">LayerWatch · 层哨</h1>

<p align="center">
  <a href="https://github.com/zhf883680/LayerWatch">https://github.com/zhf883680/LayerWatch</a><br>
  <a href="README.md">简体中文</a> | <a href="README.en.md">English</a>
</p>

依赖 Home Assistant 的 Bambu Lab A1 打印监控服务：

- 使用 HA 中指定的摄像头实体抓图，不再管理 RTSP。
- AI 异常检测：炒面、堵头/积料、打印件位移、喷嘴碰撞。
- 延时视频：与 AI 共用同一批抽帧，打印结束后自动编码 MP4。
- 触发：按当前层变化，或按固定秒数；实体由用户显式指定，不做自动发现。
- 通知：HA 持久通知与 Bark 可同时启用，不需要 Webhook 或图床。
- 清理：视频和现场图按天保留，出片后自动删除中间帧。
- FFmpeg、数据库、目录和编码参数均为内置默认值。
- Web 界面支持中文 / English 切换。

## 界面预览

### AI 分析

![AI 分析](img/ai-analysis.png)

### 延时视频

![延时视频](img/timelapse.png)

### 设定

![设定](img/settings.png)

## 配置

只保留必要配置：

```yaml
homeAssistant:
  baseURL: "http://homeassistant.local:8123"
  token: "HA 长期访问 Token"
  cameraEntity: "camera.bambu_lab_a1"
  layerEntity: "sensor.bambu_lab_a1_current_layer"
  statusEntity: "sensor.bambu_lab_a1_print_status"

ai:
  enabled: true
  baseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1"
  apiKey: "视觉模型 API Key"
  model: "qwen3-vl-flash"
  maxChecksPerPrint: 50

trigger:
  mode: "layer" # layer | interval
  intervalSeconds: 10 # 仅 interval 模式使用，最小 2 秒

cleanup:
  retentionDays: 7 # 0 = 永久保留

notification:
  haEnabled: true # HA 持久通知
  barkEnabled: false
  barkKey: ""
  barkBaseURL: "https://api.day.app"
  barkGroup: "LayerWatch"
  barkLevel: "" # active / timeSensitive / critical
  barkVolume: 0 # critical 音量 0-10
```

也可直接打开 Web 页面修改。HA Token、API Key 会保存在配置文件中，请限制配置文件权限。

## 使用 Docker 运行（推荐）

运行用户不需要克隆或编译源码，直接使用 Docker Hub 镜像：

```text
zhf883680/layerwatch:latest
```

支持 `amd64`、`arm64` 和 `armv7`，Docker 会自动选择设备对应的架构。

```bash
mkdir -p "$HOME/layerwatch/data"

docker run -d   --name layerwatch   --restart unless-stopped   --pull always   -p 19091:19091   -v "$HOME/layerwatch/data:/app/data"   -e TZ=Asia/Shanghai   zhf883680/layerwatch:latest
```

打开 `http://<服务 IP>:19091`，进入“设定”页填写 HA、AI、触发和通知配置。配置和数据库都会保存在：

```text
$HOME/layerwatch/data/config.yaml
$HOME/layerwatch/data/monitor.db
```

更新镜像：

```bash
docker pull zhf883680/layerwatch:latest
docker rm -f layerwatch
```

然后重新执行上面的 `docker run` 命令，`data` 目录中的数据不会丢失。

也可以使用仓库中的 `docker-compose.yml`：

```bash
docker compose pull
docker compose up -d
```

## 开发运行

```bash
go run ./cmd/server
```

本机需要安装 ffmpeg，或者通过 `FFMPEG_BINARY` 指定可执行文件。源码构建 Docker 镜像：

```bash
docker build -t layerwatch .
```

自动发布需要在 GitHub 仓库配置两个 Actions Secret：

```text
DOCKERHUB_USERNAME
DOCKERHUB_TOKEN
```

## 触发规则

### 按层触发（推荐）

1. 服务每 2 秒读取 `homeAssistant.statusEntity`。
2. 状态变为 `printing` 时自动创建打印任务。
3. 读取 `homeAssistant.layerEntity`，层号增加时为该层抓一帧。
4. 状态变为 `idle`、`finished`、`failed`、`stopped` 或 `offline` 时停止并编码。
5. 暂停时继续保留任务，恢复打印后从新层继续。

如果状态实体留空，第一次读到有效层号时自动开始；停止可通过 `POST /api/session/stop`。

### 按时间触发

打印状态变为 `printing` 后立即抓第一帧，随后每 `intervalSeconds` 秒抓一帧。暂停状态不抓图，结束时保存并编码。

## HA 实体建议

Bambu Lab 官方 HA 集成常用实体类似：

- 摄像头：`camera.<printer>_camera`
- 当前层：`sensor.<printer>_current_layer`
- 打印状态：`sensor.<printer>_print_status` 或 `sensor.<printer>_status`

实际实体名以你的 HA「开发者工具 → 状态」为准。服务不会猜路径或实体。

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

旧 LapseCam 的 `/api/quick/start`、`/api/quick/stop`、`/api/quick/snapshot?layer=N` 和 `/api/quick/check` 仍然兼容。

## AI 判断与告警

每次抓帧后，服务取最近 5 帧一起分析。以下情况会向已启用的 HA/Bark 渠道发送通知：

- AI 判断为异常；
- 置信度不低于 0.8；
- 连续 3 次异常；
- 距上次告警至少 5 分钟。

每个打印任务最多调用 AI `maxChecksPerPrint` 次，设为 `0` 表示不限制。

## 数据

默认数据目录为 `./data`：

```text
data/
  monitor.db
  frames/session-<id>/
  videos/
  events/session-<id>/
```

自动清理每 6 小时执行，按 `cleanup.retentionDays` 删除旧视频和现场图；成功编码后立即删除中间帧。
