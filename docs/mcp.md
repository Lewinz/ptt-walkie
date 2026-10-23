# MCP Server（控制面）

灵矽智能体通过 MCP 调用本服务完成建群、切群、回放等操作；工具返回适合 TTS 的一句话中文，同时附带 `structuredContent`。

## 接入方式

| 方式 | 地址 | 适用 |
|---|---|---|
| Streamable HTTP | `POST {BASE_URL}/mcp/d/{device_id}` | 灵矽「自定义 MCP」/ MCP Inspector / 任意 MCP 客户端 |
| Streamable HTTP（头部携带身份） | `POST {BASE_URL}/mcp` + `X-Device-Id: {device_id}` | 平台支持自定义请求头时 |

支持协议版本 `2025-11-25`、`2025-06-18`、`2025-03-26`、`2024-11-05`（按客户端请求协商）。设置 `PTT_AUTH_TOKEN` 后需携带 `Authorization: Bearer <token>` 或 `?token=<token>`。

### 设备身份

MCP 工具必须知道「我」是哪台对讲机。按以下优先级解析：

1. 工具参数 `device_id`（未在 schema 中声明，便于调试）或 `tools/call` 的 `_meta.device_id`
2. 路径 `/mcp/d/{device_id}`
3. 请求头 `X-Device-Id`
4. 查询参数 `?device_id=`

> 灵矽平台是否在 MCP 调用中携带设备 ID 尚待确认（见 requirements 第 0 步）。在确认前，**每台设备对应一个智能体、MCP 地址里写死 device_id** 是最稳妥的做法。

### 本地调试

```bash
make run
npx @modelcontextprotocol/inspector  # Transport: Streamable HTTP, URL: http://localhost:8080/mcp/d/demo-1
```

## 工具一览

| 工具 | 触发话术示例 | 说明 |
|---|---|---|
| `create_group` | 建个群叫京沪线车队 / 建一个附近的群 / 建个跑沪蓉线的群 | 返回房间号（逐字中文读法，如「九六二五」） |
| `join_group` | 加入房间九六二五 / 加入附近服务区那个群 | 房间号优先；也可按名称加入附近 / 路线群 |
| `switch_group` | 切到老乡群 / 去车队那个 / 下一个频道 | 名称模糊匹配，歧义时返回候选 |
| `list_my_groups` | 我有哪些群 | 含在线人数与当前群 |
| `get_group_status` | 我在哪个群 / 群里有谁在线 / 谁在说话 | |
| `list_nearby_groups` | 附近有什么群 | 依赖设备 GPS 上报 |
| `list_route_groups` | 有没有跑沪蓉线的群 | |
| `leave_group` | 退出这个群 | 退出后回落到其他群 |
| `mute_group` | 这个群先静音 / 静音一小时 / 取消静音 | |
| `listen_leader_only` | 只听车队长的 | |
| `replay_last` | 刚才说的再放一遍 | 在对讲机上回放原声 |
| `who_spoke_last` | 刚才是谁说的 | 有转写时附带内容 |
| `send_text_message` | 给车队群发：前面堵车 | 文字留言，在线成员设备播报 |
| `share_group` | 房间号是多少 / 怎么拉人进群 | 返回房间号、链接、二维码地址 |
| `dissolve_group` | 解散这个群 | 仅车队长 |

## 推荐智能体提示词

`initialize` 响应的 `instructions` 已包含以下要点，灵矽智能体的角色设定建议同步配置：

```
你是车载对讲机的语音助手，用户多为正在开车的司机。
1. 建群、入群、切群、查在线、静音、回放、摘要等操作必须调用工具，不要编造群名、房间号或在线人数。
2. 房间号原样传给工具（用户可能说「三八二五」「洞两拐」）。
3. 回复必须非常简短（一句话），直接复述工具返回的文本。
4. 切群有歧义时，把候选群名念给用户选择。
5. 不要主动闲聊，避免分散驾驶注意力。
```
