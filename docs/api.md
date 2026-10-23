# REST API

所有接口返回 JSON。发起设备可通过请求体 `device_id`、请求头 `X-Device-Id` 或查询参数 `device_id` 指定（按此优先级）。

## 群组

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/groups` | 所有群（含在线人数、当前发言人） |
| POST | `/api/groups` | 建群 `{name, owner_id, type: temp\|fixed\|emergency, visibility: code\|nearby\|route, route, lat, lng}`；附近群缺省坐标时取群主最近上报位置 |
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

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/groups/{id}/recordings?limit=20` | 最近发言（新→旧），含发言人、时长、转写、预警词 |
| GET | `/api/groups/{id}/recordings?since=RFC3339` | 某时刻之后的发言（旧→新），用于「我错过了什么」 |
| GET | `/api/recordings/{id}` | 单条元数据 |
| GET | `/api/recordings/{id}/audio.ogg` | 音频下载 / 浏览器直接播放 |
| POST | `/api/devices/{id}/replay` | 在设备上回放 `{n, group_id?, utterance_id?}`：按时间顺序实时推送，排除本机发言；对讲实时语音会打断回放；设备离线或无可回放内容返回 409 |
