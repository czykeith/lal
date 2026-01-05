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
  "rtsp_mode": 0                                     // RTSP 模式，0=TCP，1=UDP，默认 0
}
```

**注意：** 
- 转推模式下，`auto_stop_pull_after_no_out_ms` 参数会被忽略，系统会自动将其设置为 -1（不自动停止拉流），因为转推的目的是将流推送到远程服务器，而不是为了本地观看。
- `timeout_ms` 和 `retry_num` 参数同时应用于拉流和推流操作。
- 转推模式下，数据不会落盘，直接转发到目标服务器，以提高性能和减少磁盘I/O。

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
    "retry_num": -1
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

### GB28181 API

GB28181 功能支持通过 SIP 信令协议与国标设备进行通信，实现设备注册、心跳、拉流和停止拉流等操作。

#### 配置说明

首先需要在配置文件中启用 GB28181 功能：

```json
{
  "gb28181": {
    "enable": true,
    "local_sip_id": "34020000002000000001",      // 本地SIP ID（20位国标编码）
    "local_sip_ip": "192.168.1.100",            // 本地SIP IP地址
    "local_sip_port": 5060,                     // 本地SIP端口（默认5060）
    "local_sip_domain": "34020000002000000001",  // 本地SIP域（可选，默认使用local_sip_id）
    "username": "",                               // 认证用户名（可选）
    "password": "",                               // 认证密码（可选）
    "expires": 3600                               // 注册过期时间（秒，默认3600）
  }
}
```

**注意事项：**
- `local_sip_id` 必须是20位数字的国标编码
- `local_sip_ip` 应该设置为服务器实际IP地址，设备会向此地址发送SIP信令
- `local_sip_port` 默认使用5060，确保防火墙开放此端口（UDP）
- 如果使用Docker部署，需要映射UDP端口：`-p 5060:5060/udp`

#### 启动GB28181拉流

**接口地址：** `POST /api/ctrl/gb28181_invite`

**请求参数：**

```json
{
  "device_id": "34020000001320000001",    // 设备ID（国标编码，必填）
  "channel_id": "34020000001320000001",   // 通道ID（国标编码，必填）
  "stream_name": "test_stream",           // 流名称（可选，默认使用 device_id_channel_id）
  "port": 0,                               // RTP接收端口（可选，0表示自动分配）
  "is_tcp_flag": 0                         // 是否使用TCP传输（0=UDP，1=TCP，默认0）
}
```

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
# 使用 curl 启动GB28181拉流
curl -X POST http://127.0.0.1:8083/api/ctrl/gb28181_invite \
  -H "Content-Type: application/json" \
  -d '{
    "device_id": "34020000001320000001",
    "channel_id": "34020000001320000001",
    "stream_name": "test_stream",
    "port": 0,
    "is_tcp_flag": 0
  }'
```

**工作流程：**
1. 服务器向设备发送 SIP INVITE 请求
2. 设备响应 200 OK，开始推送 RTP 流
3. 服务器在指定端口接收 RTP 流并解析为音视频数据
4. 流可以通过 RTMP、RTSP、HLS、HTTP-FLV 等协议播放

#### 停止GB28181拉流

**接口地址：** `POST /api/ctrl/gb28181_bye`

**请求参数：**

```json
{
  "device_id": "34020000001320000001",    // 设备ID（国标编码，必填）
  "channel_id": "34020000001320000001",   // 通道ID（国标编码，必填）
  "stream_name": "test_stream"            // 流名称（可选）
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
    "device_id": "34020000001320000001",
    "channel_id": "34020000001320000001",
    "stream_name": "test_stream"
  }'
```

#### 查询GB28181设备列表

**接口地址：** `GET /api/stat/gb28181_devices`

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
curl http://127.0.0.1:8083/api/stat/gb28181_devices
```

**设备状态说明：**
- `status`: 设备状态，`online` 表示在线，`offline` 表示离线
- `register_time`: 设备注册时间
- `keepalive_time`: 最后心跳时间
- `channels`: 通道列表，每个通道包含通道ID、名称、状态和流名称

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

### 其他常用 API

#### 查询所有流信息

**接口地址：** `GET /api/stat/all_group`

**使用示例：**

```bash
curl http://127.0.0.1:8083/api/stat/all_group
```

#### 查询指定流信息

**接口地址：** `GET /api/stat/group?stream_name=test_stream`

**使用示例：**

```bash
curl "http://127.0.0.1:8083/api/stat/group?stream_name=test_stream"
```

#### 启动拉流

**接口地址：** `POST /api/ctrl/start_relay_pull`

**请求参数：**

```json
{
  "url": "rtmp://example.com/live/stream",
  "stream_name": "test_stream",
  "pull_timeout_ms": 10000,
  "pull_retry_num": -1,
  "rtsp_mode": 0
}
```

#### 停止拉流

**接口地址：** `GET /api/ctrl/stop_relay_pull?stream_name=test_stream`

#### 踢出会话

**接口地址：** `POST /api/ctrl/kick_session`

**请求参数：**

```json
{
  "stream_name": "test_stream",
  "session_id": "RTMPPUBSUB..."
}
```

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
