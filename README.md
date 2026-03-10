# LAL

[![Platform](https://img.shields.io/badge/platform-linux%20%7C%20windows%20%7C%20macos-green.svg)](https://github.com/q191201771/lal)
[![Release](https://img.shields.io/github/tag/q191201771/lal.svg?label=release)](https://github.com/q191201771/lal/releases)
[![CI](https://github.com/q191201771/lal/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/q191201771/lal/actions/workflows/ci.yml)
[![goreportcard](https://goreportcard.com/badge/github.com/q191201771/lal)](https://goreportcard.com/report/github.com/q191201771/lal)
![wechat](https://img.shields.io/:微信-q191201771-blue.svg)
![qqgroup](https://img.shields.io/:QQ群-635846365-blue.svg)

[中文文档](https://pengrl.com/lal/#/)

LAL is an audio/video live streaming broadcast server written in Go. It's sort of like `nginx-rtmp-module`, but easier to use and with more features, e.g RTMP, RTSP(RTP/RTCP), HLS, HTTP[S]/WebSocket[s]-FLV/TS, GB28181, H264/H265/AAC/G711/OPUS, relay, cluster, record, HTTP API/Notify/WebUI, GOP cache.

## Install

There are 3 ways of installing lal:

### 1. Building from source

First, make sure that Go version >= 1.18

For Linux/macOS user:

```shell
$git clone https://github.com/q191201771/lal.git
$cd lal
$make build
```

Then all binaries go into the `./bin/` directory. That's it.

For an experienced gopher(and Windows user), the only thing you should be concern is that `the main function` is under the `./app/lalserver` directory. So you can also:

```shell
$git clone https://github.com/q191201771/lal.git
$cd lal/app/lalserver
$go build
```

Or using whatever IDEs you'd like.

So far, the only direct and indirect **dependency** of lal is [naza(A basic Go utility library)](https://github.com/q191201771/lal.git) which is also written by myself. This leads to less dependency or version manager issues.

### 2. Prebuilt binaries

Prebuilt binaries for Linux, macOS(Darwin), Windows are available in the [lal github releases page](https://github.com/q191201771/lal/releases). Naturally, using [the latest release binary](https://github.com/q191201771/lal/releases/latest) is the recommended way. The naming format is `lal_<version>_<platform>.zip`, e.g. `lal_v0.20.0_linux.zip`

LAL could also be built from the source wherever the Go compiler toolchain can run, e.g. for other architectures including arm32 and mipsle which have been tested by the community.

### 3. Docker

option 1, using prebuilt image at docker hub, so just run:

```
$docker run -it -p 1935:1935 -p 8080:8080 -p 4433:4433 -p 5544:5544 -p 8083:8083 -p 8084:8084 -p 30000-30100:30000-30100/udp q191201771/lal /lal/bin/lalserver -c /lal/conf/lalserver.conf.json
```

option 2, build from local source with Dockerfile, and run:

```
$git clone https://github.com/q191201771/lal.git
$cd lal
$docker build -t lal .
$docker run -it -p 1935:1935 -p 8080:8080 -p 4433:4433 -p 5544:5544 -p 8083:8083 -p 8084:8084 -p 30000-30100:30000-30100/udp lal /lal/bin/lalserver -c /lal/conf/lalserver.conf.json
```

option 3, Use docker-compose

Create a `docker-compose.yml` file with the following content:

```yaml
version: "3.9"
services:
    lalserver:
    image: q191201771/lal
    container_name: lalserver
    ports:
        - "1935:1935"
        - "8080:8080"
        - "4433:4433"
        - "5544:5544"
        - "8083:8083"
        - "8084:8084"
        - "30000-30100:30000-30100/udp"
    command: /lal/bin/lalserver -c /lal/conf/lalserver.conf.json
```

Run the following command to start the service:

```bash
docker-compose up
```

Or run it in the background with:

```bash
docker-compose up -d
```

## Using

Running lalserver:

```
$./bin/lalserver -c ./conf/lalserver.conf.json
```

Using whatever clients you are familiar with to interact with lalserver.

For instance, publish rtmp stream to lalserver via ffmpeg:

```shell
$ffmpeg -re -i demo.flv -c:a copy -c:v copy -f flv rtmp://127.0.0.1:1935/live/test110
```

Play multi protocol stream from lalserver via ffplay:

```shell
$ffplay rtmp://127.0.0.1/live/test110
$ffplay rtsp://127.0.0.1:5544/live/test110
$ffplay http://127.0.0.1:8080/live/test110.flv
$ffplay http://127.0.0.1:8080/hls/test110/playlist.m3u8
$ffplay http://127.0.0.1:8080/hls/test110/record.m3u8
$ffplay http://127.0.0.1:8080/hls/test110.m3u8
$ffplay http://127.0.0.1:8080/live/test110.ts
```

## HTTP API

LAL 提供了丰富的 HTTP API 接口，用于控制和管理流媒体服务。默认 API 地址为 `http://127.0.0.1:8083`（可在配置文件中修改）。

### 接口索引

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/lal.html` | Web 控制台页面 |
| GET | `/api/stat/lal_info` | 查询 LAL 服务信息（版本、启动时间等） |
| GET | `/api/stat/all_group` | 查询所有流分组统计（含 pub/sub/pull 等） |
| GET | `/api/stat/group` | 查询指定流分组统计（query: stream_name） |
| POST | `/api/ctrl/start_relay_pull` | 启动拉流（仅拉流到本地，可多协议播放） |
| GET | `/api/ctrl/stop_relay_pull` | 停止拉流（query: stream_name） |
| POST | `/api/ctrl/kick_session` | 踢出指定会话（stream_name + session_id） |
| POST | `/api/ctrl/start_rtp_pub` | 启动 RTP 收流（已废弃，请用 gb28181_invite） |
| POST | `/api/ctrl/add_ip_blacklist` | 添加 IP 黑名单（ip + duration_sec） |
| POST | `/api/ctrl/start_relay` | 启动转推（拉流+推流） |
| GET | `/api/ctrl/stop_relay` | 停止转推（query: stream_name） |
| POST | `/api/ctrl/start_relay_from_stream` | 从已有流转推（仅推流，不额外拉流） |
| POST | `/api/ctrl/gb28181_invite` | GB28181 拉流 |
| POST | `/api/ctrl/gb28181_bye` | GB28181 停止拉流/回放 |
| POST | `/api/ctrl/gb28181_playback` | GB28181 回放 |
| POST | `/api/ctrl/gb28181_playback_scale` | GB28181 回放倍速控制（发送控制命令） |
| POST | `/api/ctrl/gb28181_ptz` | GB28181 云台控制 |
| GET | `/api/ctrl/gb28181_devices` | 查询 GB28181 设备列表 |
| GET | `/api/ctrl/gb28181_streams` | 查询 GB28181 流列表 |
| POST | `/api/ctrl/gb28181_device_info` | 查询 GB28181 设备信息 |
| POST | `/api/ctrl/gb28181_device_status` | 查询 GB28181 设备状态 |
| POST | `/api/ctrl/gb28181_channels` | 查询 GB28181 通道列表 |

### 转推 API

转推功能支持从 RTSP 或 RTMP 拉流，然后转推到 RTMP 或 RTSP。

#### 启动转推

**接口地址：** `POST /api/ctrl/start_relay`

**请求参数：**

```json
{
  "pull_url": "rtsp://example.com/stream",           // 拉流地址，支持 rtmp:// 或 rtsp://
  "push_url": "rtmp://example.com/live/stream",     // 推流地址，支持 rtmp:// 或 rtsp://
  "stream_name": "test_stream",                      // 流名称（可选，如果不提供则从 pull_url 解析）
  "timeout_ms": 10000,                               // 拉流和推流的超时时间（毫秒），默认 10000
  "retry_num": -1,                                   // 拉流和推流的重试次数，-1表示永远重试，大于0表示重试次数，0表示不重试，默认 0
  "rtsp_mode": 0,                                    // RTSP 模式，0=TCP，1=UDP，默认 0
  "scale": 1.0                                       // RTSP拉流时的播放速度倍数，合法范围 [1,8]；1=正常速度，>1=加速（例如2.0=2倍速）
}
```

**注意：** 
- 转推模式下，`auto_stop_pull_after_no_out_ms` 参数会被忽略，系统会自动将其设置为 -1（不自动停止拉流），因为转推的目的是将流推送到远程服务器，而不是为了本地观看。
- `timeout_ms` 和 `retry_num` 参数同时应用于拉流和推流操作。
- 转推模式下，数据不会落盘，直接转发到目标服务器，以提高性能和减少磁盘I/O。
- `scale` 参数仅对 RTSP 拉流有效，用于控制播放速度倍数。统一使用代码实现倍速，通过调整时间戳间隔来实现倍速效果，不依赖RTSP服务器是否支持Scale头：
  - 合法取值范围为 **[1,8]**，`1.0` 表示正常速度；
  - 当 `scale>1` 时启用变速播放，`scale<=1` 时按正常速度直通；
  - 如果拉流地址是 RTMP，此参数会被忽略。

**响应示例：**

```json
{
  "error_code": 0,
  "desp": "succ",
  "data": {
    "stream_name": "test_stream",
    "pull_session_id": "RTMPPULL...",
    "push_session_id": "RTMPPUSH..."
  }
}
```

**使用示例：**

```bash
# 使用 curl 启动转推
curl -X POST http://127.0.0.1:8083/api/ctrl/start_relay \
  -H "Content-Type: application/json" \
  -d '{
    "pull_url": "rtsp://example.com/stream",
    "push_url": "rtmp://example.com/live/stream",
    "stream_name": "test_stream",
    "timeout_ms": 10000,
    "retry_num": -1,
    "scale": 1.0
  }'
```

#### 停止转推

**接口地址：** `GET /api/ctrl/stop_relay?stream_name=test_stream`

**请求参数：**
- `stream_name`: 流名称（必填）

**响应示例：**

```json
{
  "error_code": 0,
  "desp": "succ",
  "data": {
    "stream_name": "test_stream",
    "pull_session_id": "RTMPPULL...",
    "push_session_id": "RTMPPUSH..."
  }
}
```

**使用示例：**

```bash
# 使用 curl 停止转推
curl "http://127.0.0.1:8083/api/ctrl/stop_relay?stream_name=test_stream"
```

#### 从已有流转推（仅推流，不额外拉流）

当流已经在本机存在（例如通过 RTMP/RTSP 推流、GB28181 接入、或自定义接入），可以直接将该流再转推到其他 RTMP/RTSP 服务器，而无需再从对方拉流。

**接口地址：** `POST /api/ctrl/start_relay_from_stream`

**请求参数：**

```json
{
  "stream_name": "test_stream",                     // 本机已有的流名称（必填）
  "push_url": "rtmp://example.com/live/stream",    // 推流地址，支持 rtmp:// 或 rtsp://（必填）
  "timeout_ms": 10000,                              // 推流超时时间（毫秒，可选，默认 10000）
  "retry_num": 0                                    // 推流重试次数：-1=永远重试，>0=重试次数，0=不重试（默认 0）
}
```

**响应示例：**

```json
{
  "error_code": 0,
  "desp": "succ",
  "data": {
    "stream_name": "test_stream",
    "push_session_id": "RTMPPUSH..."
  }
}
```

**使用示例：**

```bash
curl -X POST http://127.0.0.1:8083/api/ctrl/start_relay_from_stream \
  -H "Content-Type: application/json" \
  -d '{
    "stream_name": "test_stream",
    "push_url": "rtmp://example.com/live/stream",
    "timeout_ms": 10000,
    "retry_num": 0
  }'
```

### GB28181 API

GB28181 功能支持通过 SIP 信令协议与国标设备进行通信，实现设备注册、心跳、拉流、回放、录像查询和停止拉流等操作。信令与 SDP 格式对齐主流可播放实现，以提升设备兼容性。

**功能特性：**
- ✅ 设备注册和注销（支持 Digest 认证）
- ✅ 设备心跳保活和状态监控（基于客户端实际心跳间隔动态确定超时阈值）
- ✅ 主动拉流（INVITE）和停止拉流（BYE）
- ✅ 主码流/子码流/第三码流选择（通过 stream_index 参数）
- ✅ 视频回放（支持时间范围指定；Subject/SDP 兼容常见设备）
- ✅ 视频参数配置（编码格式、分辨率、码率、帧率、Profile/Level）
- ✅ PTZ 云台控制（方向、缩放、聚焦、光圈、预置位、巡航）
- ✅ 设备信息查询（DeviceInfo）、设备状态查询（DeviceStatus）
- ✅ 通道列表查询（Catalog，支持定时自动查询）
- ✅ 录像文件列表查询（RecordInfo，按时间范围查询，支持 200 OK 响应解析）
- ✅ NOTIFY 处理：目录状态（Catalog 事件 ON/OFF/ADD/DEL/UPDATE 等）、移动位置（MobilePosition 经纬度）
- ✅ MESSAGE 按 CmdType 分发：DeviceInfo（更新设备名称/厂商/型号）、Alarm（告警状态并回复 XML）
- ✅ 按设备+通道查找（FindChannel）、流信息查询
- ✅ NAT 兼容：Invite/Playback/Catalog 优先使用设备 RemoteIP/RemotePort 发送信令
- ✅ 自动编码转换（GBK/GB2312 ↔ UTF-8）
- ✅ 设备超时检测和离线标记（自动清理流信息和通道状态）
- ✅ 设备重连自动恢复拉流
- ✅ 服务重启后自动要求设备重新注册
- ✅ 详细的调试日志输出

#### 配置说明

首先需要在配置文件中启用 GB28181 功能：

```json
{
  "gb28181": {
    "enable": true,
    "allow_non_standard_device_id": false,        // 是否允许非20位设备ID注册（默认false；遇到如 "83010" 这类非标ID可置 true 兼容）
    "local_sip_id": "34020000002000000001",      // 本地SIP ID（20位国标编码）
    "local_sip_ip": "192.168.1.100",            // 本地SIP IP地址
    "local_sip_port": 5060,                     // 本地SIP端口（默认5060）
    "local_sip_domain": "34020000002000000001",  // 本地SIP域（可选，默认使用local_sip_id）
    "username": "admin",                         // 认证用户名（可选，启用Digest认证时必填）
    "password": "123456",                        // 认证密码（可选，启用Digest认证时必填）
    "expires": 3600,                            // 注册过期时间（秒，默认3600）
    "catalog_query_interval": 300,               // Catalog查询间隔（秒，0表示不启用定时查询）
    "sip_rtp_port_min": 30000,                  // SIP收流端口范围最小值（默认30000）
    "sip_rtp_port_max": 60000,                   // SIP收流端口范围最大值（默认60000）
    "video_codec": "H264",                       // 视频编码格式：H264/H265（默认H264）
    "video_width": 1920,                         // 视频宽度（分辨率，默认0表示不指定）
    "video_height": 1080,                        // 视频高度（分辨率，默认0表示不指定）
    "video_bitrate": 2048,                       // 视频码率（kbps，默认0表示不指定）
    "video_framerate": 25,                       // 视频帧率（fps，默认0表示不指定）
    "video_profile": "main",                     // H264 Profile：baseline/main/high（默认不指定）
    "video_level": "4.0"                         // H264 Level：如3.1、4.0等（默认不指定）
  }
}
```

**配置项说明：**

**基础配置：**
- `local_sip_id`：必须是20位数字的国标编码
- `local_sip_ip`：应该设置为服务器实际IP地址，设备会向此地址发送SIP信令
- `local_sip_port`：默认使用5060，确保防火墙开放此端口（UDP）
- `local_sip_domain`：本地SIP域，可选，默认使用 `local_sip_id`
- `username` / `password`：Digest认证用户名和密码，可选。如果配置了，服务器将启用 Digest 认证。设备注册时会先收到 401 挑战，然后使用配置的用户名和密码进行认证。未配置时，设备可以直接注册，无需认证。
- `expires`：注册过期时间（秒），默认3600秒

**端口配置：**
- `sip_rtp_port_min` / `sip_rtp_port_max`：SIP收流端口范围，用于接收RTP媒体流。默认30000-60000。如果使用Docker部署，需要映射UDP端口范围：`-p 30000-60000:30000-60000/udp`
- `rtp_port_min` / `rtp_port_max`：已废弃，请使用 `sip_rtp_port_min` 和 `sip_rtp_port_max`（向后兼容）

**定时查询配置：**
- `catalog_query_interval`：Catalog查询间隔（秒），0表示不启用定时查询。启用后，系统会定期查询所有在线设备的通道列表，保持通道信息及时更新。

**视频参数配置（用于INVITE请求的SDP）：**
- `video_codec`：视频编码格式，支持 `H264` 或 `H265`，默认 `H264`
- `video_width` / `video_height`：视频分辨率，默认0表示不指定（由设备决定）
- `video_bitrate`：视频码率（kbps），默认0表示不指定
- `video_framerate`：视频帧率（fps），默认0表示不指定
- `video_profile`：H264 Profile，支持 `baseline`、`main`、`high`，默认不指定
- `video_level`：H264 Level，如 `3.1`、`4.0` 等，默认不指定

**注意事项：**
- 如果使用 Docker 部署，需要映射 SIP 端口和 RTP 端口范围：
  ```bash
  -p 5060:5060/udp -p 30000-60000:30000-60000/udp
  ```
- **设备状态监控**：服务器会根据设备实际心跳间隔动态确定超时阈值。设备离线时会自动清理相关流信息和通道状态。
- **服务重启恢复**：服务重启后，若收到未注册设备的心跳，会返回 401 要求设备重新注册。
- **NOTIFY**：支持设备上报 NOTIFY（目录状态 Catalog、移动位置 MobilePosition），自动更新通道状态与经纬度。
- **MESSAGE**：支持设备上报 MESSAGE 中的 DeviceInfo、Alarm 等，自动更新设备信息并按要求回复（如告警回复 XML）。

#### 启动GB28181拉流

**接口地址：** `POST /api/ctrl/gb28181_invite`

**请求参数：**

```json
{
  "device_id": "34020000001320000001",    // 设备ID（国标编码，必填）
  "channel_id": "34020000001320000001",   // 通道ID（国标编码，必填）
  "stream_name": "test_stream",           // 流名称（必填，全局唯一）
  "port": 0,                              // RTP接收端口（保留字段，当前统一自动分配，建议填 0）
  "is_tcp_flag": 0,                       // 是否使用TCP传输（0=UDP，1=TCP，默认0）
  "stream_index": 0                       // 码流索引（可选；常见：0=主，1=子，2=第三...；不传默认 0=主码流）
}
```

**海康相关说明：**
- 海康/NVR 常见按“码流索引”选流：主码流传 `stream_index=0`，子码流传 `stream_index=1`（如有第三码流可尝试 `2`）。
- 本项目会在 INVITE 中同步携带 Subject/SDP 的索引扩展以提升兼容性（常见写法：`Subject: 通道ID:0,平台ID:0`，以及 SDP 的 `a=streamprofile:<index>` / `a=streamid:<index>`）。

**响应示例：**

```json
{
  "error_code": 0,
  "desp": "succ",
  "data": {
    "stream_name": "test_stream",
    "session_id": "PSPUB...",
    "port": 30000
  }
}
```

**使用示例：**

```bash
# 使用 curl 启动GB28181拉流（默认 stream_index=1：子码流）
curl -X POST http://127.0.0.1:8083/api/ctrl/gb28181_invite \
  -H "Content-Type: application/json" \
  -d '{
    "device_id": "34020000001320000001",
    "channel_id": "34020000001320000001",
    "stream_name": "test_stream",
    "port": 0,
    "is_tcp_flag": 0
  }'

# 拉主码流（stream_index=0）
curl -X POST http://127.0.0.1:8083/api/ctrl/gb28181_invite \
  -H "Content-Type: application/json" \
  -d '{
    "device_id": "34020000001320000001",
    "channel_id": "34020000001320000001",
    "stream_name": "test_stream_main",
    "port": 0,
    "is_tcp_flag": 0,
    "stream_index": 0
  }'
```

**工作流程：**
1. 服务器向设备发送 SIP INVITE 请求
2. 设备响应 200 OK，开始推送 RTP 流
3. 服务器在指定端口接收 RTP 流并解析为音视频数据
4. 流可以通过 RTMP、RTSP、HLS、HTTP-FLV 等协议播放

#### GB28181回放（符合GB28181标准协议）

**接口地址：** `POST /api/ctrl/gb28181_playback`

**功能说明：**
本接口实现符合GB28181国家标准的视音频文件回放功能，通过SIP INVITE方法建立回放会话，在SDP消息体中指定回放参数。

**请求参数：**

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| device_id | string | 是 | 设备ID（国标 20 位编码） |
| channel_id | string | 是 | 通道ID（国标 20 位编码） |
| stream_name | string | 是 | 流名称（全局唯一，用于拉流/停止标识） |
| start_time | string | 是 | 开始时间，见下方时间格式说明 |
| end_time | string | 是 | 结束时间，须晚于 start_time |
| port | int | 否 | RTP 接收端口，0 表示自动分配（默认 0） |
| is_tcp_flag | int | 否 | 0=UDP，1=TCP（默认 0） |
| stream_index | int | 否 | 码流索引：0=主码流，1=子码流，2=第三码流…（默认 0） |

**时间格式：** `2006-01-02T15:04:05` 或 `2006-01-02 15:04:05`。无时区时按**服务器本地时区**解析（如东八区），避免与北京时间差 8 小时；也可使用带时区的 RFC3339（如 `2024-01-01T10:00:00+08:00`）。

**请求示例：**

```json
{
  "device_id": "34020000001320000001",
  "channel_id": "34020000001320000001",
  "stream_name": "playback_stream",
  "start_time": "2024-01-01T10:00:00",
  "end_time": "2024-01-01T11:00:00",
  "port": 0,
  "is_tcp_flag": 0,
  "stream_index": 0
}
```

**响应参数：** `data.stream_name`（与请求一致）、`data.port`（RTP 接收端口，可用于拉流）。

**响应示例：**

```json
{
  "error_code": 0,
  "desp": "succ",
  "data": {
    "stream_name": "playback_stream",
    "port": 30000
  }
}
```

**使用示例：**

```bash
# 最小必填参数
curl -X POST http://127.0.0.1:8083/api/ctrl/gb28181_playback \
  -H "Content-Type: application/json" \
  -d '{
    "device_id": "34020000001320000001",
    "channel_id": "34020000001320000001",
    "stream_name": "playback_stream",
    "start_time": "2024-01-01T10:00:00",
    "end_time": "2024-01-01T11:00:00"
  }'

# 带子码流、TCP
curl -X POST http://127.0.0.1:8083/api/ctrl/gb28181_playback \
  -H "Content-Type: application/json" \
  -d '{
    "device_id": "34020000001320000001",
    "channel_id": "34020000001320000001",
    "stream_name": "playback_stream",
    "start_time": "2024-01-01T10:00:00",
    "end_time": "2024-01-01T11:00:00",
    "stream_index": 1,
    "is_tcp_flag": 1
  }'
```

**工作流程（符合 GB28181）：**
1. 服务器向设备发送 SIP INVITE（回放模式）
2. SDP：`s=Playback`、`u=<channelId>:0`、`t=<start> <end>`（Unix 时间）；Subject 带码流索引
3. 设备 200 OK 后推送回放 RTP 流
4. 服务器在端口接收并解析，可通过 RTMP/RTSP/HLS/HTTP-FLV 等播放

**注意事项：**
- **时间**：无时区字符串按服务器本地时区解析；`end_time` 须晚于 `start_time`
- **stream_name**：必填且全局唯一，停止回放时用同一 `stream_name` 调用 `gb28181_bye`
- **幂等**：同一 `stream_name` 已存在回放时，直接返回成功
- **NAT**：INVITE 优先发往设备 RemoteIP/RemotePort
 - **会话有效期**：默认每个回放会话有 3 小时有效期，到期后服务器会自动发送 BYE 关闭并释放资源；如需更长或更短可在服务端配置/代码中调整。

#### GB28181回放倍速控制（发送控制指令，不改变拉流逻辑）

**接口地址：** `POST /api/ctrl/gb28181_playback_scale`

**说明：** 该接口用于对已建立的回放会话（`stream_name` 对应）发送 **PlaybackControl** 控制命令来调整设备端推流速度，**不会改变本地拉流/转码逻辑**。如果回放会话未建立或已结束，会返回失败。

**请求参数：**

```json
{
  "stream_name": "playback_stream",   // 回放流名称（必填）
  "scale": 2.0                        // 倍速（必填），如 0.5/1/2/4/8
}
```

**响应示例：**

```json
{
  "error_code": 0,
  "desp": "succ",
  "data": {
    "stream_name": "playback_stream",
    "scale": 2,
    "session_id": "PSPUB..."
  }
}
```

**使用示例：**

```bash
curl -X POST http://127.0.0.1:8083/api/ctrl/gb28181_playback_scale \
  -H "Content-Type: application/json" \
  -d '{"stream_name":"playback_stream","scale":2.0}'
```

#### 停止GB28181拉流/回放

**接口地址：** `POST /api/ctrl/gb28181_bye`

**请求参数：**

```json
{
  "stream_name": "test_stream"            // 流名称（必填，全局唯一，用于定位拉流或回放会话）
}
```

**响应示例：**

```json
{
  "error_code": 0,
  "desp": "succ",
  "data": {
    "stream_name": "test_stream",
    "session_id": "PSPUB..."
  }
}
```

**使用示例：**

```bash
# 使用 curl 停止GB28181拉流
curl -X POST http://127.0.0.1:8083/api/ctrl/gb28181_bye \
  -H "Content-Type: application/json" \
  -d '{
    "stream_name": "test_stream"
  }'
```

#### 查询GB28181设备列表

**接口地址：** `GET /api/ctrl/gb28181_devices`

**响应示例：**

```json
{
  "error_code": 0,
  "desp": "succ",
  "data": {
    "devices": [
      {
        "device_id": "34020000001320000001",
        "device_name": "34020000001320000001",
        "status": "online",
        "register_time": "2024-01-01 10:00:00",
        "keepalive_time": "2024-01-01 10:05:00",
        "channels": [
          {
            "channel_id": "34020000001320000001",
            "channel_name": "通道1",
            "status": "streaming",
            "stream_name": "test_stream"
          }
        ]
      }
    ]
  }
}
```

**使用示例：**

```bash
# 使用 curl 查询设备列表
curl http://127.0.0.1:8083/api/ctrl/gb28181_devices
```

**设备状态说明：**
- `status`: 设备状态，`online` 表示在线，`offline` 表示离线，`alarmed` 表示告警（收到 Alarm 时更新）
- `register_time`: 设备注册时间
- `keepalive_time`: 最后心跳时间
- `manufacturer` / `model`: 设备厂商与型号（收到 MESSAGE DeviceInfo 时更新）
- `channels`: 通道列表，每个通道包含通道 ID、名称、状态、流名称；若设备上报 NOTIFY MobilePosition，通道可包含经纬度（longitude/latitude）

#### PTZ 控制（云台控制）

**接口地址：** `POST /api/ctrl/gb28181_ptz`

**请求参数：**

```json
{
  "device_id": "34020000001320000001",    // 设备ID（国标编码，必填）
  "channel_id": "34020000001320000001",   // 通道ID（国标编码，必填）
  "command": "Up",                        // PTZ命令（必填）
  "speed": 5,                             // 速度 1-8（可选，默认5，仅用于方向控制）
  "preset": 1                             // 预置位编号（可选，用于预置位相关命令）
}
```

**支持的命令：**
- **方向控制**：`Up`（上）、`Down`（下）、`Left`（左）、`Right`（右）、`UpLeft`（左上）、`UpRight`（右上）、`DownLeft`（左下）、`DownRight`（右下）
- **缩放控制**：`ZoomIn`（放大）、`ZoomOut`（缩小）
- **聚焦控制**：`FocusNear`（聚焦+）、`FocusFar`（聚焦-）
- **光圈控制**：`IrisOpen`（光圈+）、`IrisClose`（光圈-）
- **停止**：`Stop`（停止所有PTZ操作）
- **预置位**：`SetPreset`（设置预置位）、`CallPreset`（调用预置位）、`DelPreset`（删除预置位）
- **巡航**：`StartCruise`（开始巡航）、`StopCruise`（停止巡航）

**响应示例：**

```json
{
  "error_code": 0,
  "desp": "succ",
  "data": {
    "device_id": "34020000001320000001",
    "channel_id": "34020000001320000001",
    "command": "Up"
  }
}
```

**使用示例：**

```bash
# 云台向上移动
curl -X POST http://127.0.0.1:8083/api/ctrl/gb28181_ptz \
  -H "Content-Type: application/json" \
  -d '{
    "device_id": "34020000001320000001",
    "channel_id": "34020000001320000001",
    "command": "Up",
    "speed": 5
  }'

# 调用预置位1
curl -X POST http://127.0.0.1:8083/api/ctrl/gb28181_ptz \
  -H "Content-Type: application/json" \
  -d '{
    "device_id": "34020000001320000001",
    "channel_id": "34020000001320000001",
    "command": "CallPreset",
    "preset": 1
  }'

# 停止PTZ操作
curl -X POST http://127.0.0.1:8083/api/ctrl/gb28181_ptz \
  -H "Content-Type: application/json" \
  -d '{
    "device_id": "34020000001320000001",
    "channel_id": "34020000001320000001",
    "command": "Stop"
  }'
```

#### 查询设备信息

**接口地址：** `POST /api/ctrl/gb28181_device_info`

**请求参数：**

```json
{
  "device_id": "34020000001320000001"    // 设备ID（国标编码，必填）
}
```

**响应示例：**

```json
{
  "error_code": 0,
  "desp": "succ",
  "data": {
    "device_id": "34020000001320000001",
    "device_name": "设备名称",
    "manufacturer": "厂商名称",
    "model": "设备型号",
    "firmware": "固件版本"
  }
}
```

**使用示例：**

```bash
curl -X POST http://127.0.0.1:8083/api/ctrl/gb28181_device_info \
  -H "Content-Type: application/json" \
  -d '{
    "device_id": "34020000001320000001"
  }'
```

**注意：** 此接口会向设备发送查询请求，设备信息需要从设备的响应中获取。如果设备未响应或响应格式不正确，部分字段可能为空。

#### 查询设备状态

**接口地址：** `POST /api/ctrl/gb28181_device_status`

**请求参数：**

```json
{
  "device_id": "34020000001320000001"    // 设备ID（国标编码，必填）
}
```

**响应示例：**

```json
{
  "error_code": 0,
  "desp": "succ",
  "data": {
    "device_id": "34020000001320000001",
    "status": "online"
  }
}
```

**使用示例：**

```bash
curl -X POST http://127.0.0.1:8083/api/ctrl/gb28181_device_status \
  -H "Content-Type: application/json" \
  -d '{
    "device_id": "34020000001320000001"
  }'
```

#### 查询通道列表

**接口地址：** `POST /api/ctrl/gb28181_channels`

**请求参数：**

```json
{
  "device_id": "34020000001320000001"    // 设备ID（国标编码，必填）
}
```

**响应示例：**

```json
{
  "error_code": 0,
  "desp": "succ",
  "data": {
    "device_id": "34020000001320000001",
    "channels": [
      {
        "channel_id": "34020000001320000001",
        "channel_name": "通道1",
        "status": "idle",
        "stream_name": ""
      },
      {
        "channel_id": "34020000001320000002",
        "channel_name": "通道2",
        "status": "streaming",
        "stream_name": "test_stream"
      }
    ]
  }
}
```

**使用示例：**

```bash
curl -X POST http://127.0.0.1:8083/api/ctrl/gb28181_channels \
  -H "Content-Type: application/json" \
  -d '{
    "device_id": "34020000001320000001"
  }'
```

**注意：** 此接口会向设备发送通道列表查询请求。如果设备已注册，会返回缓存的通道列表；如果缓存不存在，会主动查询设备并更新缓存。

#### 查询流列表

**接口地址：** `GET /api/ctrl/gb28181_streams`

**响应示例：**

```json
{
  "error_code": 0,
  "desp": "succ",
  "data": {
    "streams": [
      {
        "device_id": "34020000001320000001",
        "channel_id": "34020000001320000001",
        "stream_name": "test_stream",
        "call_id": "1234567890@192.168.1.100",
        "port": 30000,
        "is_tcp": false,
        "start_time": "2024-01-01 10:00:00"
      }
    ]
  }
}
```

**使用示例：**

```bash
curl http://127.0.0.1:8083/api/ctrl/gb28181_streams
```

**流信息说明：**
- `device_id`: 设备ID
- `channel_id`: 通道ID
- `stream_name`: 流名称
- `call_id`: SIP Call-ID
- `port`: RTP接收端口
- `is_tcp`: 是否使用TCP传输
- `start_time`: 流开始时间

#### 播放GB28181流

启动拉流后，可以通过以下方式播放流：

```bash
# RTMP播放
ffplay rtmp://127.0.0.1/live/test_stream

# RTSP播放
ffplay rtsp://127.0.0.1:5544/live/test_stream

# HTTP-FLV播放
ffplay http://127.0.0.1:8080/live/test_stream.flv

# HLS播放
ffplay http://127.0.0.1:8080/hls/test_stream/playlist.m3u8
```

### 统计与查询 API

#### 查询 LAL 服务信息

**接口地址：** `GET /api/stat/lal_info`

**说明：** 返回服务版本、二进制信息、API 版本、启动时间等。

**响应示例：**

```json
{
  "error_code": 0,
  "desp": "succ",
  "data": {
    "server_id": "...",
    "bin_info": "...",
    "lal_version": "vx.x.x",
    "api_version": "...",
    "notify_version": "...",
    "start_time": "2024-01-01 10:00:00"
  }
}
```

**使用示例：**

```bash
curl http://127.0.0.1:8083/api/stat/lal_info
```

#### 查询所有流信息

**接口地址：** `GET /api/stat/all_group`

**说明：** 返回当前所有流分组统计。每个分组包含 `stream_name`、`app_name`、音视频编码/分辨率、`pub`（推流端会话，含 RTMP/RTSP/GB28181 等）、`subs`（拉流/播放会话）、`pull`（拉流会话）、`in_frame_per_sec` 等。GB28181 拉流会在 `pub` 中展示，含 `read_bytes_sum`、`read_bitrate_kbits`、`bitrate_kbits` 等。

**响应示例：**

```json
{
  "error_code": 0,
  "desp": "succ",
  "data": {
    "groups": [
      {
        "stream_name": "test_stream",
        "app_name": "live",
        "audio_codec": "AAC",
        "video_codec": "H264",
        "video_width": 1920,
        "video_height": 1080,
        "pub": {
          "session_id": "PSPUB...",
          "protocol": "PS",
          "base_type": "pub",
          "read_bytes_sum": 1234567,
          "read_bitrate_kbits": 1024,
          "bitrate_kbits": 1024,
          "start_time": "..."
        },
        "subs": [],
        "pull": { ... }
      }
    ]
  }
}
```

**使用示例：**

```bash
curl http://127.0.0.1:8083/api/stat/all_group
```

#### 查询指定流信息

**接口地址：** `GET /api/stat/group?stream_name=test_stream`

**请求参数：**
- `stream_name`（必填）：流名称

**说明：** 返回指定流的分组统计，结构同单条 `all_group` 中的元素。若流不存在则 `error_code` 为 1001（group not found）。

**响应示例：** 与 `all_group` 中单个 group 结构一致，外层为 `{"error_code":0,"desp":"succ","data":{ ... }}`。

**使用示例：**

```bash
curl "http://127.0.0.1:8083/api/stat/group?stream_name=test_stream"
```

### 拉流与会话控制 API

#### 启动拉流

**接口地址：** `POST /api/ctrl/start_relay_pull`

**说明：** 从指定 URL（RTMP 或 RTSP）拉流到本地，拉流成功后可通过 RTMP/RTSP/HLS/HTTP-FLV 等协议播放。与转推不同，不向其他地址推流。

**请求参数：**

```json
{
  "url": "rtmp://example.com/live/stream",         // 拉流地址（必填），支持 rtmp:// 或 rtsp://
  "stream_name": "test_stream",                     // 流名称（可选，不填则从 url 解析）
  "pull_timeout_ms": 10000,                         // 拉流超时（毫秒），默认 10000
  "pull_retry_num": -1,                             // 重试次数：-1=永远，0=不重试，>0=次数
  "auto_stop_pull_after_no_out_ms": -1,             // 无观众自动停止拉流（毫秒），-1=不自动停止，0=立即，>0=持续该毫秒后停止
  "rtsp_mode": 0,                                   // RTSP 模式，0=TCP，1=UDP（仅 rtsp:// 有效）
  "debug_dump_packet": "",                           // 调试落盘路径（可选）
  "scale": 1.0                                      // RTSP 拉流倍速，1.0=正常，2.0=2 倍速等
}
```

**响应示例：**

```json
{
  "error_code": 0,
  "desp": "succ",
  "data": {
    "stream_name": "test_stream",
    "session_id": "RTMPPULL..."
  }
}
```

**使用示例：**

```bash
curl -X POST http://127.0.0.1:8083/api/ctrl/start_relay_pull \
  -H "Content-Type: application/json" \
  -d '{"url":"rtmp://example.com/live/stream","stream_name":"test_stream","pull_retry_num":-1}'
```

#### 停止拉流

**接口地址：** `GET /api/ctrl/stop_relay_pull?stream_name=test_stream`

**请求参数：**
- `stream_name`（必填）：流名称

**响应示例：**

```json
{
  "error_code": 0,
  "desp": "succ",
  "data": {
    "session_id": "RTMPPULL..."
  }
}
```

**使用示例：**

```bash
curl "http://127.0.0.1:8083/api/ctrl/stop_relay_pull?stream_name=test_stream"
```

#### 踢出会话

**接口地址：** `POST /api/ctrl/kick_session`

**说明：** 根据流名称和会话 ID 踢出指定会话（如某个播放端或推流端）。

**请求参数：**

```json
{
  "stream_name": "test_stream",    // 流名称（必填）
  "session_id": "RTMPPUBSUB..."    // 会话 ID（必填），可从 /api/stat/group 或 /api/stat/all_group 的 pub/subs 中获取
}
```

**响应示例：**

```json
{
  "error_code": 0,
  "desp": "succ"
}
```

**使用示例：**

```bash
curl -X POST http://127.0.0.1:8083/api/ctrl/kick_session \
  -H "Content-Type: application/json" \
  -d '{"stream_name":"test_stream","session_id":"RTMPPUBSUB..."}'
```

#### 添加 IP 黑名单

**接口地址：** `POST /api/ctrl/add_ip_blacklist`

**说明：** 将指定 IP 加入黑名单，在有效期内该 IP 的请求会被拒绝。

**请求参数：**

```json
{
  "ip": "192.168.1.100",    // IP 地址（必填）
  "duration_sec": 3600      // 生效时长（秒，必填）
}
```

**响应示例：**

```json
{
  "error_code": 0,
  "desp": "succ"
}
```

**使用示例：**

```bash
curl -X POST http://127.0.0.1:8083/api/ctrl/add_ip_blacklist \
  -H "Content-Type: application/json" \
  -d '{"ip":"192.168.1.100","duration_sec":3600}'
```

#### 启动 RTP 收流（已废弃）

**接口地址：** `POST /api/ctrl/start_rtp_pub`

**说明：** 该接口已废弃。RTP 收流请使用 GB28181 方式：在配置中启用 GB28181 后，通过 `POST /api/ctrl/gb28181_invite` 拉流。调用本接口会返回错误提示使用 `gb28181_invite`。

---

更多 API 文档请参考：https://pengrl.com/lal/#/HTTPAPI

## More than a server, act as package and client

Besides a live stream broadcast server which named `lalserver` precisely, `project lal` even provides many other applications, e.g. push/pull/remux stream clients, bench tools, examples. Each subdirectory under the `./app/demo` directory represents a tiny demo.

Our goals are not only a production server but also a simple package with a well-defined, user-facing API, so that users can build their own applications on it.

`LAL` stands for `Live And Live` if you may wonder.


## Contact

Bugs, questions, suggestions, anything related or not, feel free to contact me with [lal github issues](https://github.com/q191201771/lal/issues).

## License

MIT, see [License](https://github.com/q191201771/lal/blob/master/LICENSE).

this note updated by yoko, 202404
