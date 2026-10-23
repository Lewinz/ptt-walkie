# ptt-walkie

灵矽语音对讲机 (PTT walkie-talkie on Lingxi AI)

面向司机（但不限于）的[灵矽 AI](https://xrobo.qiniu.com/#/agent-market) 应用：通过多种方式灵活建群，用嵌入式设备进行群语音对讲，并支持语音切换群（房间）。

| 模拟对讲机 | 车队管理 | 扫码入群 |
|---|---|---|
| ![device](docs/assistant.png) | ![admin](docs/admin-console.png) | ![join](docs/join-page.png) |

## 架构

```
            ┌──────────── 控制面 ─────────────┐
 "切到老乡群" → 灵矽智能体 (ASR+LLM) ──MCP──▶ ┐
                                             │   ptt-server（单个 Go 二进制）
 管理端 / 扫码页 ─────────── REST ─────────▶ ├─ store：群 / 设备 / 房间号 / 模板
                                             ├─ mcp：19 个对讲工具（HTTP + 接入点反连）
            ┌──────────── 语音面 ─────────────┤─ voice：抢麦、优先级仲裁、Opus 转发
 按住 PTT ═══ Opus 16k/60ms ═══ WebSocket ══▶ ├─ archive：Ogg Opus 存档、回放
 ◀══════════ 群内其他设备实时播放 ═══════════ ├─ ai：转写、关键词预警、错过摘要、离线补收
                                             └─ lifecycle：临时群自动解散
```

- **控制面**：灵矽智能体通过 MCP 调用建群、入群、切群、回放、摘要等工具；切群结果实时推送到设备。
- **语音面**：自建 WebSocket 对讲转发，裸 Opus 帧（与 xiaozhi-esp32 一致），服务端抢麦锁与优先级仲裁（紧急频道 > 车队长 > 成员）。
- **管理面**：车队管理 H5、扫码入群页、批量导入设备；浏览器模拟对讲机用于无硬件演示。

## 快速开始

```bash
make run                      # 或 docker compose up --build
open http://localhost:8080/device   # 开两个标签页 = 两台对讲机
open http://localhost:8080/admin    # 车队管理
```

1. 在一个标签页「开机」→ 建群（或对 AI 助手说「建个群叫京沪线车队」），得到房间号。
2. 另一个标签页输入房间号加入，或在管理端扫二维码。
3. 按住橙色按钮（或空格）讲话，另一台实时播放；同时按下会听到「嘟嘟」（抢麦失败）。
4. 对 AI 助手说「切到老乡群」「我错过了什么」「刚才说的再放一遍」。

> 浏览器只在 HTTPS 或 localhost 下开放麦克风；局域网 / 公网演示请配置 `PTT_TLS_CERT` / `PTT_TLS_KEY` 或放在 HTTPS 反向代理之后。模拟器需要支持 WebCodecs 的 Chrome / Edge。

### 接入灵矽智能体

把 `https://<你的域名>/mcp/d/<设备ID>` 配置为智能体的 MCP 服务（或设置 `PTT_MCP_ENDPOINT` 让服务端主动连接平台的 MCP 接入点），提示词见 [docs/mcp.md](docs/mcp.md)。

## 配置

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `PTT_ADDR` | `:8080` | 监听地址 |
| `PTT_DATA_DIR` | `./data` | 状态快照与录音目录 |
| `PTT_BASE_URL` | `http://localhost:8080` | 对外地址：分享链接、二维码、ASR 拉取录音 |
| `PTT_MAX_TALK` | `60s` | 单次发言最长时间 |
| `PTT_TEMP_GROUP_IDLE` | `2h` | 临时群空闲解散时间（模板可覆盖） |
| `PTT_NEARBY_RADIUS_KM` | `3` | 附近群默认半径 |
| `PTT_AI_KEY` | — | 七牛 AI 大模型推理 API Key；配置后启用服务端转写、LLM 摘要与助手 |
| `PTT_AI_BASE_URL` | `https://api.qnaigc.com/v1` | OpenAI 兼容推理地址 |
| `PTT_AI_MODEL` | `deepseek-v3` | 摘要 / 助手使用的模型 |
| `PTT_ASR_MODE` | `qiniu` | `qiniu`（`/voice/asr` 传录音 URL）或 `openai`（`/audio/transcriptions` 上传） |
| `PTT_ASR_MODEL` | `asr` | ASR 模型名 |
| `PTT_ALERT_WORDS` | 事故,查车,封路,… | 全局预警词（逗号分隔，模板另有补充） |
| `PTT_AUTH_TOKEN` | — | `/mcp` 访问令牌（Bearer 或 `?token=`） |
| `PTT_MCP_ENDPOINT` | — | MCP 接入点：`wss://…?token=…\|设备ID; …` |
| `PTT_TLS_CERT` / `PTT_TLS_KEY` | — | 启用 HTTPS |
| `PTT_KODO_BUCKET` / `PTT_KODO_AK` / `PTT_KODO_SK` | — | 配置后录音同步到七牛 Kodo（S3 兼容接口），ASR 改用预签名链接，**服务端无公网地址也能转写** |
| `PTT_KODO_ENDPOINT` | `https://s3.cn-east-1.qiniucs.com` | Kodo S3 域名，区域自动从域名解析（或 `PTT_KODO_REGION`） |
| `PTT_KODO_PREFIX` | `ptt-walkie/` | 对象键前缀 |

## 文档

- [需求与产品设计](docs/requirements.md) · [黑客松实施计划](docs/hackathon-plan.md)
- [设备对讲协议](docs/protocol.md) — 固件对接（WebSocket + Opus）
- [固件接入指南](docs/firmware.md) — ESP32 双通道 / 音频仲裁 / 联调工具 / 性能实测
- [MCP Server](docs/mcp.md) — 工具列表、设备身份、灵矽接入、内置助手
- [REST API](docs/api.md) — 群组、设备、存档回放、AI、模板

## 开发

```bash
make test   # go test -race ./...
make vet
```

设备模拟器：`go run ./cmd/ptt-sim -mode echo|play|load -code <房间号>`（回声测试 / 循环播放 / 压测）。

代码结构：`cmd/ptt-server` 入口、`cmd/ptt-sim` 设备模拟器；`internal/{store,api,voice,archive,mcp,ai,assistant,lifecycle,config}`；`web/static` 前端（`go:embed` 打包）。
