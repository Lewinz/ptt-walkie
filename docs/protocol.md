# 设备对讲协议（语音面）

设备与对讲转发服务之间使用一条 WebSocket 长连接：

```
ws(s)://<host>/ws?device_id=<设备唯一 ID>&name=<显示名，可选>
```

- **文本帧**：JSON 信令，`type` 字段区分消息。
- **二进制帧**：一帧即一个裸 Opus 包，**16kHz、单声道、60ms**（与 xiaozhi-esp32 协议一致，固件可直接复用其编解码管线）。
- 同一 `device_id` 只保留最新连接，旧连接收到 `notice` 后被关闭。
- 服务端每 20s 发送 WebSocket Ping；客户端也可以发 `{"type":"ping"}`。

## 典型时序

```
设备A                     服务端                      设备B
  │── ptt_start ───────────▶│                            │
  │◀── floor{granted:true} ─│                            │
  │══ opus ════════════════▶│── talk_start ─────────────▶│
  │══ opus ════════════════▶│══ opus ═══════════════════▶│ 播放
  │── ptt_stop ────────────▶│── talk_stop ──────────────▶│
```

抢麦失败时返回 `floor{granted:false, reason:"busy", holder_name}`，设备本地播放「嘟嘟」提示音，**不做语音播报**，避免干扰驾驶。

## 设备 → 服务端

| type | 字段 | 说明 |
|---|---|---|
| `hello` | `name?` | 可选；更新显示名并重新获取 `welcome` |
| `ptt_start` | | 按下 PTT，申请当前群的话权 |
| `ptt_stop` | | 松开 PTT，释放话权 |
| `switch` | `group_id` / `query` / `delta` | 切群：精确 ID、名称或房间号（模糊）、`delta: 1/-1` 对应旋钮下一个 / 上一个 |
| `location` | `lat`, `lng` | 上报 GPS，用于附近群 |
| `pair` | | 「碰一碰」配对键：两台设备 8 秒内都按下（双方有 GPS 时还需相距 300m 内）即自动建两人临时群；失败时收到 `notice` |
| `transcript` | `utterance_id`, `text` | 可选：发言者自带的转写（浏览器 Web Speech API）；仅在服务端未配置 ASR 时采纳，且只接受发言者本人提交 |
| `ping` | | 应用层心跳，回 `pong` |
| *二进制* | Opus 包 | 仅在持有话权时转发，否则丢弃 |

## 服务端 → 设备

| type | 关键字段 | 说明 |
|---|---|---|
| `welcome` | `device`, `audio`, `max_talk`, `announce` | 连接建立后的设备完整状态与编码参数 |
| `channel` | `group`, `reason`, `announce` | 当前频道变化（含语音 / MCP / 管理端触发的切群、群解散）；`announce` 如「已切换到老乡群，5人在线」，供设备 TTS 或提示 |
| `floor` | `granted`, `utterance_id`, `reason`, `holder_name`, `max_ms` | 抢麦结果；`reason`: `busy` / `no_group` |
| `floor_revoked` | `reason`, `holder_name` | 话权被收回：`preempted`（车队长 / 紧急频道插话）、`max_duration`（超时）、`idle`（4s 无音频） |
| `talk_start` | `utterance_id`, `group_id`, `group_name`, `speaker_id`, `speaker_name`, `leader`, `priority`, `replay?` | 下行一段语音开始，随后的二进制帧都属于该段 |
| `talk_stop` | `utterance_id`, `reason`, `duration_ms` | 该段结束；`reason: preempted` 表示被更高优先级语音打断 |
| `presence` | `group_id`, `online_count`, `online`, `member_count` | 当前频道在线成员变化 |
| `transcript` | `utterance_id`, `speaker_name`, `text`, `alert` | 一段发言的转写（字幕），`alert` 为命中的预警词 |
| `invite` | `invite{group_id, group_name, code, from_name}`, `announce` | 有人邀请本机入群；设备可提示并在用户确认后调用 `POST /api/devices/{id}/accept`，或对智能体说「接受邀请」 |
| `notice` | `level: info\|alert`, `text` | 短文本通知（群解散、关键词预警等），设备可 TTS 播报；`alert` 级别应先播放预警音 |
| `error` | `code?`, `message`, `candidates?` | 错误；切群名称有歧义时 `code: ambiguous` 并给出候选名 |
| *二进制* | Opus 包 | 当前 `talk_start` 对应语音段的音频 |

## 优先级与仲裁

设备端**同一时刻只播放一路下行语音**，由服务端仲裁：

| 优先级 | 来源 |
|---|---|
| 3 | 紧急频道（`type: emergency`）——即使设备当前不在该频道，只要是成员也会收到 |
| 2 | 车队长（群内 `leader` 角色）——可抢占普通成员的话权 |
| 1 | 普通成员 |

高优先级语音到达时，服务端先对正在播放的段发送 `talk_stop{reason:"preempted"}`，再发送新的 `talk_start`。

过滤规则：
- 静音的群（`mute`）不下发任何语音。
- 开启「只听车队长」的群只下发 `leader: true` 的语音。
- 正在发射的设备不接收同群语音（半双工），紧急语音除外。

## 设备端音频仲裁建议

| 输入 | 行为 |
|---|---|
| 长按 PTT | 对讲：`ptt_start` → 采集 Opus 上行 → 松开 `ptt_stop` |
| 短按 / 唤醒词 | 进入灵矽 AI 对话（建群、切群、回放、摘要等走 MCP） |
| 收到 `talk_start` | 立即打断 AI 播报，播放对讲语音 |
| 旋钮 | `switch{delta:±1}` |
