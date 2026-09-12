<p align="center">
  <img src="web/layerwatch-512.png" width="150" alt="LayerWatch">
</p>

<h1 align="center">LayerWatch · 层哨</h1>

<p align="center">
  <a href="https://github.com/zhf883680/LayerWatch">https://github.com/zhf883680/LayerWatch</a><br>
  <a href="README.md">简体中文</a> | <a href="README.en.md">English</a>
</p>

> **LayerWatch 通过 Home Assistant 摄像头与 AI 视觉分析监控 Bambu Lab 3D 打印过程，及时发现炒面、堵头、位移和碰撞等异常，并自动生成延时视频。**

- 使用 HA 中指定的摄像头实体抓图，不再管理 RTSP。
- AI 异常检测：炒面、堵头/积料、打印件位移、喷嘴碰撞。
- 延时视频：与 AI 共用同一批抽帧，打印结束后自动编码 MP4，网页内可直接预览。
- 触发：按当前层变化，或按固定秒数；实体由用户显式指定，不做自动发现。
- 通知：HA 持久通知与 Bark 可同时启用，不需要 Webhook 或图床。
- 照明：可在抓图前自动打开 HA 照明实体，支持打印期间保持亮灯。
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
  cameraEntity: "camera.bambu_lab_camera"
  layerEntity: "sensor.bambu_lab_current_layer"
  statusEntity: "sensor.bambu_lab_print_status"

ai:
  enabled: true
  baseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1"
  apiKey: "视觉模型 API Key"
  model: "qwen3-vl-flash"
  maxImageWidth: 640 # 发给 AI 前把帧缩到该宽度，0 = 不压缩；缩图最省 token
  imageSource: "base64" # base64 | temp（阿里云百炼临时文件 URL，请求体更小）
  maxChecksPerPrint: 50 # 每个任务最多分析次数，0 = 不限制
  minIntervalSeconds: 30 # 两次 AI 分析的最小间隔，0 = 不限制

trigger:
  mode: "layer" # layer | interval
  intervalSeconds: 10 # 仅 interval 模式使用，最小 2 秒

cleanup:
  retentionDays: 7 # 0 = 永久保留

lighting:
  enabled: false
  entity: "light.bambu_lab_chamber_light"
  delaySeconds: 3
  keepOnDuringPrint: true

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
2. 状态变为 `printing`、`running` 或 `prepare` 时自动创建打印任务（不同版本的 HA 集成取值不同，服务只在状态变化时打印一次日志）。
3. 读取 `homeAssistant.layerEntity`，层号增加时为该层抓一帧。
4. 状态变为 `idle`、`finished`、`failed`、`stopped` 或 `offline` 时停止并编码。
5. 暂停时继续保留任务，恢复打印后从新层继续。

如果状态实体留空，第一次读到有效层号时自动开始；停止可通过 `POST /api/session/stop`。

### 按时间触发

打印状态变为 `printing`、`running` 或 `prepare` 后立即抓第一帧，随后每 `intervalSeconds` 秒抓一帧。暂停状态不抓图，结束时保存并编码。

## HA 实体建议

Bambu Lab 官方 HA 集成常用实体类似：

- 摄像头：`camera.<printer>_camera`
- 当前层：`sensor.<printer>_current_layer`
- 打印状态：`sensor.<printer>_print_status` 或 `sensor.<printer>_status`

实际实体名以你的 HA「开发者工具 → 状态」为准。服务不会猜路径或实体。

## 夜间照明

打印机弱光环境下，AI 可能无法判断打印状态。可在“设定 → 照明控制”配置 HA 照明实体：

```yaml
lighting:
  enabled: true
  entity: "light.bambu_lab_chamber_light"
  delaySeconds: 3
  keepOnDuringPrint: true
```

行为说明：

- `delaySeconds`：抓图前提前打开照明并等待指定秒数，让摄像头自动曝光稳定。
- `keepOnDuringPrint: true`：第一次抓图时开灯，并保持到打印任务结束。
- `keepOnDuringPrint: false`：仅在每次抓图前开灯，抓图完成后立即关灯。
- 如果照明原本已经打开，LayerWatch 不会在任务结束时把它关闭。
- 照明控制失败不会阻止摄像头抓图，只会记录错误日志。

### 照明测试页

“设定 → 照明控制 → 照明测试”会打开 `http://<主机>:19091/light-test`。这个页面用来实测开灯以后多久才能截到变亮的画面，帮你确定 `delaySeconds` 该设多少：

- 页面上的“设定延迟”默认取当前 `lighting.delaySeconds`；
- 点一次按钮跑一轮：关灯取暗底 → 开灯 → 按设定延迟抓第一张 → 之后按间隔连续抓图，直到画面变亮或超时；
- 结果显示实测延迟（开灯 → 首张变亮）、设定延迟是否够用、亮度曲线和截图对比；
- 改掉时间再点一次，就能对比不同设定，测试用的截图保存在 `data/light-tests/`（只保留最近 8 轮）。

判定“变亮”的方式是画面平均亮度超过暗底加一个阈值（暗底亮度的 25%，最少 4、最多 15 个亮度点）。测试默认在结束后把灯关回原状；勾选“测试结束后保持亮灯”可以在打印过程中测试而不影响补光。

## API

```text
GET    /api/status
GET    /api/config
PUT    /api/config
POST   /api/test/ha
POST   /api/test/ai
POST   /api/test/notification
POST   /api/test/light
POST   /api/test/light-capture?delayMs=300&intervalMs=300&timeoutMs=20000&leaveLightOn=0
GET    /api/test/light-capture
DELETE /api/test/light-capture
GET    /api/test/light-capture/image/{run}/{name}
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

每次抓帧后，服务取最近 3 帧一起分析。以下情况会向已启用的 HA/Bark 渠道发送通知：

- AI 判断为异常；
- 置信度不低于 0.8；
- 连续 3 次异常；
- 距上次告警至少 5 分钟。

每个打印任务最多调用 AI `maxChecksPerPrint` 次，设为 `0` 表示不限制。

`minIntervalSeconds` 是两次 AI 分析之间的最小间隔（默认 30 秒，`0` = 不限制）：层号变化很快时，帧照常抓取，但不会连续打 AI，省钱也避免刷屏。

### 省 token

任务一开始就持续分析，不提供按任务开关（`analyzeFrames`、`detail`、`disableThinking` 也已写死，不再可配）：

| 固定行为 | 说明 |
| --- | --- |
| 每次 3 帧 | 取最近 3 帧一起分析，帧数直接决定图片 token |
| `detail: auto` | 图片细节交给模型自取，兼顾小瑕疵识别与 token |
| 默认关闭思考模式 | 仅对阿里云端点生效，避免又慢又计费的 thinking token |

可配置的省 token 旋钮：

| 配置 | 作用 |
| --- | --- |
| `maxImageWidth` | 发送前把帧缩到该宽度（默认 640，`0` = 不压缩）；缩图是最稳定的省 token 手段 |
| `imageSource: temp` | 先把图传到阿里云百炼临时 OSS，再把 `oss://` URL 发给模型，请求体从几百 KB 降到几十字节（同一帧按内容 sha256 缓存，不重复上传；临时 URL 48 小时有效，仅适合个人/测试） |
| `maxChecksPerPrint` / `minIntervalSeconds` | 限制单任务总次数与最小间隔，避免层号快速变化时连打 AI |

日志里会打印每次调用的 token 用量，便于核对缓存命中：

```text
[ai] token 用量: 输入=1736 输出=56 缓存命中=512
```


## AI 费用估算

以下按照 `qwwen-3.7-flash` 的价格计算：

- 正常输入：0.2 元 / 百万 token
- 缓存命中输入：正常输入价格的 10%，即 0.2 × 10% = 0.02 元 / 百万 token
- 输出：0.8 元 / 百万 token

每次调用按 2000 token 估算，其中输出为 56 token：

```text
输入 = 2000 - 56 = 1944 token
实际缓存命中比例 = 512 ÷ 1680 ≈ 30.48%
折算后缓存命中输入 ≈ 1944 × 30.48% ≈ 592 token
折算后非缓存输入 ≈ 1944 - 592 = 1352 token
```

单次 AI 调用费用：

```text
输出：56 ÷ 1,000,000 × 0.8 = 0.0000448 元
缓存输入：592 ÷ 1,000,000 × 0.02 = 0.00001184 元
非缓存输入：1352 ÷ 1,000,000 × 0.2 = 0.0002704 元

单次合计：
0.0000448 + 0.00001184 + 0.0002704 ≈ 0.000327 元
```

一次 3D 打印按 30 次 AI 调用估算：

```text
0.000327 × 30 = 0.00981 元
```

**结论：一次 3D 打印的 AI 预估费用约为 0.0098 元，也就是约 0.98 分钱，不到 1 分钱。**

如果直接按实际用量 1736 token 计算，而不是按 2000 token 估算，一次打印约为 **0.00866 元，约 0.87 分钱**。

> 实际费用会根据模型价格、图片尺寸、缓存命中率和每次分析帧数变化，以上仅用于估算。

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
