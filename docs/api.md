# REST API

所有接口返回 JSON。发起设备可通过请求体 `device_id`、请求头 `X-Device-Id` 或查询参数 `device_id` 指定（按此优先级）。

## 群组

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/groups` | 所有群（含在线人数、当前发言人） |
| POST | `/api/groups` | 建群 `{name, owner_id, template, type: temp\|fixed\|emergency, visibility: code\|nearby\|route, talk_policy: all\|leaders, route, lat, lng}`；附近群缺省坐标时取群主最近上报位置 |
| GET | `/api/templates` | 场景模板列表 |
| GET | `/api/groups/{id}` | 群详情 |
| DELETE | `/api/groups/{id}` | 解散（带设备身份时须为车队长；不带视为管理端） |
| GET | `/api/groups/{id}/qr.png` | 入群二维码（内容为 `{BASE_URL}/join/{code}`） |
| POST | `/api/groups/{id}/join` | 入群并切换为当前频道 `{device_id, name}` |
| POST | `/api/groups/{id}/leave` | 退群；群主退群时车队长身份移交给最早加入者 |
| POST | `/api/groups/{id}/mute` | 静音 `{device_id, minutes}`，0 取消、负数永久 |
| POST | `/api/groups/{id}/leader-only` | 只听车队长 `{device_id, on}` |
| PUT | `/api/groups/{id}/members/{device}/role` | 设置角色 `{actor_id, role: leader\|member}` |
| GET | `/api/codes/{code}` | 按房间号查群，支持「三七二幺」等中文数字 |
| POST | `/api/codes/{code}/join` | 按房间号入群 |
| GET | `/api/nearby?lat=&lng=&radius_km=` | 附近群；缺省坐标时使用设备最近上报位置 |
| GET | `/api/routes?q=` | 路线群检索（如「沪蓉」「G42」） |

## 设备

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/devices/{id}` | 设备状态：当前群、群列表、静音设置、在线状态 |
| PUT | `/api/devices/{id}` | 注册 / 改名 `{name}` |
| POST | `/api/devices/{id}/location` | 上报位置 `{lat, lng}` |
| POST | `/api/devices/{id}/switch` | 切群：`{group_id}`、`{query}`（名称模糊或房间号）、`{delta: 1\|-1}`（下一个 / 上一个）；名称歧义时返回 409 与候选列表 |

## 发言存档与回放

每段 PTT 发言（≥3 帧，过滤误触）存为标准 Ogg Opus 文件（`$PTT_DATA_DIR/recordings/{group}/{utterance}.ogg`），索引为 JSON Lines（`index.jsonl`，后写覆盖先写，便于追加转写结果）。

配置 `PTT_KODO_*` 后，每段录音异步上传到七牛 Kodo（S3 兼容接口，自研 SigV4，路径风格 `{endpoint}/{bucket}/{prefix}{group}/{utterance}.ogg`），记录 `remote` 字段为对象键；ASR 使用 1 小时有效的预签名链接；本地文件被清理后下载接口 302 到 Kodo。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/groups/{id}/recordings?limit=20` | 最近发言（新→旧），含发言人、时长、转写、预警词 |
| GET | `/api/groups/{id}/recordings?since=RFC3339` | 某时刻之后的发言（旧→新），用于「我错过了什么」 |
| GET | `/api/recordings/{id}` | 单条元数据 |
| GET | `/api/recordings/{id}/audio.ogg` | 音频下载 / 浏览器直接播放 |
| POST | `/api/devices/{id}/replay` | 在设备上回放 `{n, group_id?, utterance_id?}`：按时间顺序实时推送，排除本机发言；对讲实时语音会打断回放；设备离线或无可回放内容返回 409 |

## 碰一碰与邀请

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/devices/{id}/pair` | 长轮询最多 8 秒等待另一台设备同时配对；成功返回新建的临时群，超时 409 |
| POST | `/api/groups/{id}/invite` | 邀请 `{from, target}`，target 为车牌 / 设备名 / 设备 ID |
| GET | `/api/devices/{id}/invites` | 待处理邀请（30 分钟有效，新→旧） |
| POST | `/api/devices/{id}/accept` | 接受邀请 `{group_id?}`，缺省接受最新一条 |

## 车队管理

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/groups/{id}/members/batch` | 批量导入设备 `{devices: [{device_id, name}], pin}`；`pin: true` 时设为设备当前频道（固定频道），否则仅加入不打断当前收听 |

## 运营统计

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/stats?hours=24` | 时间窗内（≤7 天）总通话分钟、发言次数、活跃设备 / 群、预警次数；逐小时分布；群活跃度排行；发言最多的设备 Top 10 |

## 页面

| 路径 | 说明 |
|---|---|
| `/device` | 浏览器模拟对讲机（`?device=` 指定设备 ID，`?join=` 开机后自动按房间号入群） |
| `/admin` | 车队管理：群实时状态（收听 / 讲话中）、二维码、设队长、解散、发言记录回听、批量导入 |
| `/join/{code}` | 扫码入群页：绑定实体对讲机（填设备 ID）或用当前浏览器当对讲机 |

## AI：转写、预警、摘要

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/groups/{id}/summary?minutes=30&device_id=` | 「我错过了什么」：排除本机发言；有 LLM 时生成 ≤80 字播报，否则列出最近转写，再否则按发言人统计 |

- **转写**：配置 `PTT_AI_KEY` 后，每段发言存档完成即调用七牛 `POST /v1/voice/asr`（`PTT_ASR_MODE=qiniu`，传录音公网地址，需 `PTT_BASE_URL` 可被公网访问）或 OpenAI 兼容 `/audio/transcriptions`（`PTT_ASR_MODE=openai`，直接上传）。未配置时采纳客户端自带转写。
- **关键词预警**：转写命中 `PTT_ALERT_WORDS`（默认：事故、查车、封路、追尾、堵车、危险、救命、着火）时，向该群**所有在线成员**（含正在收听其他频道的）推送 `notice{level: alert}`：设备先放预警音，再播报「注意！车队的老王说：……」。
- **离线补收**：设备断线超过 20 秒后重连，若当前群期间有他人发言：≤3 条自动补播原声，更多则播报摘要。

## 场景模板与临时群生命周期

| 模板 | 默认类型 | 发言规则 | 额外预警词 | 空闲解散 |
|---|---|---|---|---|
| `driver` 车队 | 固定 | 所有人 | 查车、限行、改道、爆胎 | — |
| `cycling` 骑行 | 临时 | 所有人 | 掉队、爆胎、摔车、等一下 | 6 小时 |
| `hiking` 登山 | 临时 | 所有人 | 迷路、受伤、掉队、下雨、天黑 | 24 小时 |
| `guide` 导游 | 临时 | **仅队长**（游客只听，抢麦返回 `listen_only`） | 集合、走散 | 12 小时 |
| `worksite` 工地班组 | 固定 | 所有人 | 受伤、塌方、漏电、起火、停电、撤离 | — |
| `family` 家庭 | 固定 | 所有人 | 救命、摔倒、不舒服、迷路、快来 | — |

- 入群时设备播报模板欢迎语，如「欢迎加入故宫一日游，请留意导游的讲解和集合通知。12人在线，本群只听不讲」。
- 临时群按模板空闲时长（未指定时 `PTT_TEMP_GROUP_IDLE`，默认 2 小时）自动解散；解散前 5 分钟向群内播报「说句话就能保留」，正在讲话的群不会解散。
