# 固件接入指南（ESP32-S3 / xiaozhi 生态）

本项目的语音面协议刻意与 [xiaozhi-esp32](https://github.com/78/xiaozhi-esp32) 的 WebSocket 音频通道保持一致（JSON 信令 + 裸 Opus 二进制帧，16kHz 单声道 60ms），固件只需**新增一条对讲连接**，复用已有的采集、编解码和播放管线。协议细节见 [protocol.md](protocol.md)。

> 下文的类名、函数名以 xiaozhi-esp32 的整体结构为参考，实际以所用 SDK 版本为准。灵矽官方 SDK 若与 xiaozhi 同源，接入方式相同。

## 双通道结构

```
            ┌───────────── 设备 ──────────────┐
 唤醒词/短按 → │ AI 通道（灵矽 / xiaozhi 原协议）  │ ⇄ 灵矽智能体（ASR→LLM→TTS，MCP 调本服务）
            │                                │
 长按 PTT  → │ 对讲通道（本协议 /ws）            │ ⇄ ptt-server（抢麦、转发、存档）
            │        ↑ 共用：麦克风 → Opus 编码  │
            │        ↓ 共用：Opus 解码 → 扬声器  │
            └────────────────────────────────┘
```

对讲通道**常驻在线**（收听频道、接收切群推送），AI 通道按需建立，两者共享音频硬件，由「音频仲裁器」决定当前谁占用麦克风和扬声器。

## 状态机

| 状态 | 进入 | 麦克风 | 扬声器 | 退出 |
|---|---|---|---|---|
| `Listening` 待机收听 | 默认 | 关 | 对讲下行 | 按 PTT / 唤醒 |
| `Requesting` 申请话权 | 长按 PTT（>200ms） | 关 | 对讲下行 | 收到 `floor` |
| `Transmitting` 发射 | `floor.granted=true`，播放「滴」 | Opus 上行到对讲通道 | 静音 | 松开 → `ptt_stop`；`floor_revoked` |
| `Denied` 抢麦失败 | `floor.granted=false` | 关 | 本地「嘟嘟」 | 松开 |
| `AIChat` AI 对话 | 唤醒词 / 短按 | 送 AI 通道 | AI 播报 | 对话结束；**收到 `talk_start` 立即打断** |

规则：
1. 对讲下行优先于 AI 播报：`talk_start` 到达时停止 TTS 播放并切到对讲解码。
2. 发射中不播放下行（半双工）；服务端也不会向发射方推送同群语音。
3. `channel` 消息到达时更新屏幕 / 指示灯，`announce` 可用本地提示音代替（ESP32 不做本地 TTS）；如设备接入灵矽，切群本就由智能体语音确认。
4. `notice{level:"alert"}` 先播放内置预警音；文字可交给 AI 通道朗读或忽略。

## 关键代码（伪代码）

```cpp
class WalkieChannel {
 public:
  void Start(const std::string& device_id) {
    ws_ = CreateWebSocket();
    ws_->OnData([this](const char* data, size_t len, bool binary) {
      if (binary) {                       // 一帧 Opus
        if (state_ != kTransmitting) audio_->PushOpusForPlayback(data, len);
        return;
      }
      auto msg = cJSON_Parse(data);
      auto type = JsonStr(msg, "type");
      if (type == "floor")          OnFloor(JsonBool(msg, "granted"));
      else if (type == "floor_revoked") StopTransmit(/*notify=*/false), Beep(kDeny);
      else if (type == "talk_start") arbiter_->PreemptAi(), ShowSpeaker(JsonStr(msg, "speaker_name"));
      else if (type == "talk_stop")  Beep(kRoger);
      else if (type == "channel")    ShowChannel(msg), Beep(kClick);
      else if (type == "notice" && JsonStr(msg, "level") == "alert") Beep(kAlert);
    });
    ws_->Connect(server_ + "/ws?device_id=" + device_id);   // 断线指数退避重连
  }

  void OnPttDown()  { state_ = kRequesting; Send(R"({"type":"ptt_start"})"); }
  void OnPttUp()    { if (state_ == kTransmitting) StopTransmit(true); state_ = kListening; }
  void OnKnob(int d){ Send(Format(R"({"type":"switch","delta":%d})", d)); }

 private:
  void OnFloor(bool granted) {
    if (!granted) { Beep(kDeny); state_ = kDenied; return; }
    if (!ptt_held_) { Send(R"({"type":"ptt_stop"})"); return; }  // 已松开
    Beep(kPermit);
    state_ = kTransmitting;
    // 复用 AI 通道相同的编码器：16kHz、单声道、60ms 帧
    audio_->StartOpusEncoding([this](std::vector<uint8_t>&& pkt) {
      ws_->Send(pkt.data(), pkt.size(), /*binary=*/true);
    });
  }
  void StopTransmit(bool notify) {
    audio_->StopOpusEncoding(/*flush=*/true);  // 先 flush，避免丢尾音
    if (notify) Send(R"({"type":"ptt_stop"})");
    state_ = kListening;
  }
};
```

## 硬件与网络

- **按键**：PTT 独立大按键（或方向盘按键），长按对讲、短按唤醒 AI；可选旋钮切频道。
- **网络**：车载建议 4G Cat.1 模组，WiFi 不可靠；弱网下服务端 4s 无音频自动释放话权，客户端断线重连后服务端会补播 / 播报摘要。
- **GPS**：周期上报 `{"type":"location"}`（如每 2 分钟或移动 >500m），用于附近群。
- **设备 ID**：建议用芯片 MAC 或出厂序列号，印在机身 / 开机播报，便于扫码页绑定。

## 联调工具

```bash
# 回声测试机器人：设备讲完后原样复读，验证上下行编解码
go run ./cmd/ptt-sim -mode echo -code <房间号>

# 循环播放一段录音（如从 /api/recordings/{id}/audio.ogg 下载），模拟群里有人说话
go run ./cmd/ptt-sim -mode play -code <房间号> -file sample.ogg -every 15s

# 转发性能：N 台设备收听，测端到端延迟与丢包
go run ./cmd/ptt-sim -mode load -code <房间号> -n 500 -dur 10s
```

本地实测（Apple M 系列，单进程）：

| 收听设备数 | 帧数 | 丢包 | p50 | p99 |
|---|---|---|---|---|
| 100 | 100 × 100 | 0% | 0.94ms | 2.4ms |
| 500 | 100 × 500 | 0% | 2.5ms | 8.9ms |
