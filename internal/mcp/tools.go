package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/plant-squad/ptt-walkie/internal/ai"
	"github.com/plant-squad/ptt-walkie/internal/api"
	"github.com/plant-squad/ptt-walkie/internal/archive"
	"github.com/plant-squad/ptt-walkie/internal/social"
	"github.com/plant-squad/ptt-walkie/internal/store"
	"github.com/plant-squad/ptt-walkie/internal/voice"
)

// Deps are the backend services the tools operate on.
type Deps struct {
	Store          *store.Store
	API            *api.Server
	Hub            *voice.Hub
	Archive        *archive.Handler
	AI             *ai.Service     // optional; enables summarize_missed
	Social         *social.Service // optional; enables pairing and invites
	NearbyRadiusKm float64
}

type tools struct{ Deps }

// NewServer builds an MCP server with the walkie-talkie tool set.
func NewServer(d Deps) *Server {
	t := &tools{d}
	s := &Server{Name: "ptt-walkie"}
	for _, tool := range []*Tool{
		{Name: "create_group", Title: "建群",
			Description: "创建一个对讲群并自动加入、切换过去。用户说「建个群叫京沪线车队」「建一个附近的群」「建个跑沪蓉线的群」时调用。返回房间号，其他人说房间号即可加入。",
			InputSchema: schema(props{
				"name":       str("群名称，如「京沪线车队」"),
				"type":       enum("群类型：temp 临时群（附近 / 路线，长时间无人说话自动解散），fixed 固定群（车队、老乡、家人）。默认 fixed；附近群默认 temp", "temp", "fixed"),
				"visibility": enum("可见性：code 仅房间号 / 扫码加入（默认），nearby 附近的人可发现，route 同路线的人可发现", "code", "nearby", "route"),
				"route":      str("visibility=route 时的路线或目的地，如「上海-成都 G42」"),
				"template":   enum("场景模板：driver 车队（默认），cycling 骑行，hiking 登山，guide 导游（只有导游能讲），worksite 工地班组，family 家庭", "driver", "cycling", "hiking", "guide", "worksite", "family"),
			}, "name"),
			handler: t.createGroup},
		{Name: "join_group", Title: "入群",
			Description: "加入一个群并切换过去。用户说「加入房间3825」「加入三八二五」「加入附近那个服务区的群」时调用。优先传房间号。",
			InputSchema: schema(props{"query": str("房间号（数字或中文数字都可以）或群名称")}, "query"),
			handler:     t.joinGroup},
		{Name: "switch_group", Title: "切群",
			Description: "切换当前对讲频道到已加入的另一个群。用户说「切到老乡群」「去车队那个」「切到3825」「下一个频道」「上一个」时调用。",
			InputSchema: schema(props{
				"query":     str("目标群名称（可模糊）或房间号；按顺序切换时留空"),
				"direction": enum("按顺序切换：next 下一个，previous 上一个", "next", "previous"),
			}),
			handler: t.switchGroup},
		{Name: "list_my_groups", Title: "我的群",
			Description: "列出我加入的所有群及在线人数。用户说「我有哪些群」「我加了几个群」时调用。",
			InputSchema: schema(props{}),
			handler:     t.listMyGroups},
		{Name: "get_group_status", Title: "当前群状态",
			Description: "查询当前所在群、在线成员、谁在说话。用户说「我在哪个群」「群里有谁在线」「现在谁在说话」时调用。",
			InputSchema: schema(props{"query": str("要查询的群，留空为当前群")}),
			handler:     t.groupStatus},
		{Name: "list_nearby_groups", Title: "附近的群",
			Description: "查找附近可加入的群（按设备最近上报的 GPS）。用户说「附近有什么群」「服务区有群吗」时调用。",
			InputSchema: schema(props{"radius_km": num("搜索半径，公里，默认 3")}),
			handler:     t.listNearby},
		{Name: "list_route_groups", Title: "同路线的群",
			Description: "按路线、高速或目的地查找同路线群。用户说「有没有跑沪蓉线的群」「去成都的群」时调用。",
			InputSchema: schema(props{"route": str("路线、高速编号或目的地，如「沪蓉」「G42」「成都」")}, "route"),
			handler:     t.listRoute},
		{Name: "leave_group", Title: "退群",
			Description: "退出一个群。用户说「退出这个群」「退出老乡群」时调用。",
			InputSchema: schema(props{"query": str("要退出的群，留空为当前群")}),
			handler:     t.leaveGroup},
		{Name: "mute_group", Title: "静音",
			Description: "临时静音某个群（不再播放该群语音）或取消静音。用户说「这个群先静音」「静音一小时」「取消静音」时调用。",
			InputSchema: schema(props{
				"query":   str("要静音的群，留空为当前群"),
				"minutes": num("静音分钟数，默认 60；0 表示取消静音；-1 表示一直静音"),
			}),
			handler: t.muteGroup},
		{Name: "listen_leader_only", Title: "只听车队长",
			Description: "开启或关闭「只听车队长的」：开启后该群只播放车队长的语音。用户说「只听车队长的」「恢复听所有人」时调用。",
			InputSchema: schema(props{
				"on":    boolean("true 开启，false 关闭"),
				"query": str("群，留空为当前群"),
			}, "on"),
			handler: t.leaderOnly},
		{Name: "replay_last", Title: "回放",
			Description: "在对讲机上重新播放群里最近的语音（不含自己说的）。用户说「刚才说的再放一遍」「重放最后三条」时调用。",
			InputSchema: schema(props{
				"n":     num("回放最近几条，默认 1，最多 10"),
				"query": str("群，留空为当前群"),
			}),
			handler: t.replayLast},
		{Name: "who_spoke_last", Title: "刚才谁说的",
			Description: "查询群里最近是谁说的话、什么时候、说了多久（有转写时附带内容）。用户说「刚才是谁说的」「最近谁说话了」时调用。",
			InputSchema: schema(props{
				"n":     num("最近几条，默认 3"),
				"query": str("群，留空为当前群"),
			}),
			handler: t.whoSpoke},
		{Name: "summarize_missed", Title: "我错过了什么",
			Description: "总结群里最近错过的语音内容（谁说了什么、路况、集合信息等）。用户说「我错过了什么」「刚才群里说了啥」「最近半小时有什么消息」时调用。",
			InputSchema: schema(props{
				"minutes": num("总结最近多少分钟，默认 30"),
				"query":   str("群，留空为当前群"),
			}),
			handler: t.summarize},
		{Name: "send_text_message", Title: "群里留言",
			Description: "把一句文字留言播报给群里所有在线成员（适合不方便按键说话时）。用户说「给车队群发：前面堵车」「告诉大家我晚点到」时调用。",
			InputSchema: schema(props{
				"text":  str("留言内容"),
				"query": str("目标群，留空为当前群"),
			}, "text"),
			handler: t.sendText},
		{Name: "share_group", Title: "分享群",
			Description: "获取群的房间号和入群链接，用于告诉别人怎么加入。用户说「房间号是多少」「怎么让别人进群」时调用。",
			InputSchema: schema(props{"query": str("群，留空为当前群")}),
			handler:     t.shareGroup},
		{Name: "pair_nearby", Title: "碰一碰配对",
			Description: "和身边的另一台对讲机配对，自动建一个两人的临时群。两台设备需要在几秒内同时发起（用户说「配对」「和旁边的车配对」「碰一碰」）。会等待最多 8 秒。",
			InputSchema: schema(props{}),
			handler:     t.pairNearby},
		{Name: "invite_member", Title: "邀请成员",
			Description: "邀请指定的人（按车牌号、设备名或设备 ID）加入群，对方设备会收到邀请提示。用户说「邀请沪A12345进车队群」「把老王拉进来」时调用。",
			InputSchema: schema(props{
				"target": str("被邀请人的车牌号、设备名或设备 ID"),
				"query":  str("邀请进哪个群，留空为当前群"),
			}, "target"),
			handler: t.inviteMember},
		{Name: "accept_invite", Title: "接受邀请",
			Description: "接受别人发来的入群邀请。用户说「接受邀请」「同意」「好的我加入」时调用。",
			InputSchema: schema(props{}),
			handler:     t.acceptInvite},
		{Name: "dissolve_group", Title: "解散群",
			Description: "解散一个群（仅车队长可操作）。用户明确说「解散这个群」时调用。",
			InputSchema: schema(props{"query": str("群，留空为当前群")}),
			handler:     t.dissolveGroup},
	} {
		s.AddTool(tool)
	}
	return s
}

// ---- schema helpers ----

type props map[string]any

func schema(p props, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": map[string]any(p)}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "number", "description": desc} }
func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
func enum(desc string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": values}
}

// ---- helpers ----

var errNoDevice = errResult("无法识别是哪台对讲机。请在 MCP 地址中携带设备 ID（/mcp/d/{device_id} 或 X-Device-Id 请求头）。")

var spokenDigit = map[rune]string{'0': "零", '1': "幺", '2': "二", '3': "三", '4': "四", '5': "五", '6': "六", '7': "拐", '8': "八", '9': "九"}

// SpokenCode renders a room code digit by digit so TTS doesn't read
// "3825" as "三千八百二十五".
func SpokenCode(code string) string {
	var b strings.Builder
	for _, r := range code {
		b.WriteString(spokenDigit[r])
	}
	return b.String()
}

func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "刚刚"
	case d < time.Hour:
		return fmt.Sprintf("%d分钟前", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d小时前", int(d.Hours()))
	}
	return t.Format("1月2日15:04")
}

func names(gs []store.GroupView) string {
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = "「" + g.Name + "」"
	}
	return strings.Join(out, "、")
}

// target resolves the group a tool acts on: the query, or the current group.
func (t *tools) target(dev, query string) (store.GroupView, *Result) {
	if strings.TrimSpace(query) == "" {
		d, err := t.Store.Device(dev)
		if err != nil || d.CurrentGroup == "" {
			r := errResult("你还没有加入任何群。可以说「建个群叫某某」或「加入房间某某某某」。")
			return store.GroupView{}, &r
		}
		g, err := t.Store.Group(d.CurrentGroup)
		if err != nil {
			r := errResult("当前群已不存在。")
			return store.GroupView{}, &r
		}
		return g, nil
	}
	res := t.Store.Resolve(dev, query)
	switch {
	case res.Group != nil:
		return *res.Group, nil
	case len(res.Candidates) > 0:
		r := Result{Text: fmt.Sprintf("找到多个群：%s，请问是哪一个？", names(res.Candidates)),
			Data: map[string]any{"candidates": res.Candidates}}
		return store.GroupView{}, &r
	}
	r := errResult("没有找到「%s」这个群。", query)
	return store.GroupView{}, &r
}

func (t *tools) statusText(st api.GroupStatus) string {
	return fmt.Sprintf("已切换到%s，%d人在线", st.Name, st.OnlineCount)
}

// ---- tool handlers ----

func (t *tools) createGroup(_ context.Context, c Caller, a Args) Result {
	if c.DeviceID == "" {
		return errNoDevice
	}
	if _, err := t.Store.EnsureDevice(c.DeviceID, ""); err != nil {
		return errResult("%v", err)
	}
	in := store.CreateGroupInput{
		Name:       a.String("name"),
		Type:       store.GroupType(a.String("type")),
		Visibility: store.Visibility(a.String("visibility")),
		Route:      a.String("route"),
		Template:   a.String("template"),
	}
	if _, ok := store.TemplateByID(in.Template); !ok {
		in.Template = ""
	}
	if in.Visibility == store.VisNearby {
		d, _ := t.Store.Device(c.DeviceID)
		if d.Location == nil {
			return errResult("还没有收到你的位置，暂时无法建附近的群。可以先建普通群，用房间号邀请。")
		}
		in.Location = d.Location
		if in.Type == "" {
			in.Type = store.GroupTemp
		}
	}
	if in.Visibility == store.VisRoute && in.Route == "" {
		in.Route = in.Name
	}
	g, err := t.Store.CreateGroup(c.DeviceID, in)
	if err != nil {
		return errResult("建群失败：%v", err)
	}
	st := t.API.Status(g)
	kind := "群"
	if g.Type == store.GroupTemp {
		kind = "临时群"
	}
	if t, ok := store.TemplateByID(g.Template); ok && t.ID != "driver" {
		kind = t.Name + kind
	}
	return Result{
		Text: fmt.Sprintf("已创建%s「%s」，房间号%s。其他人说「加入房间%s」就能进来。", kind, g.Name, SpokenCode(g.Code), SpokenCode(g.Code)),
		Data: map[string]any{"group_id": g.ID, "code": g.Code, "code_spoken": SpokenCode(g.Code), "share_url": st.ShareURL, "group": st},
	}
}

func (t *tools) joinGroup(_ context.Context, c Caller, a Args) Result {
	if c.DeviceID == "" {
		return errNoDevice
	}
	q := a.String("query")
	if q == "" {
		return errResult("请告诉我房间号或群名称。")
	}
	t.Store.EnsureDevice(c.DeviceID, "")
	res := t.Store.Resolve(c.DeviceID, q)
	var target *store.GroupView
	switch {
	case res.Group != nil:
		target = res.Group
	case len(res.Candidates) > 0:
		return Result{Text: fmt.Sprintf("找到多个群：%s，请问是哪一个？", names(res.Candidates))}
	default:
		// Not one of mine: look among publicly discoverable groups.
		var public []store.GroupView
		if d, _ := t.Store.Device(c.DeviceID); d.Location != nil {
			public = append(public, t.Store.NearbyGroups(*d.Location, t.NearbyRadiusKm)...)
		}
		public = append(public, t.Store.RouteGroups(q)...)
		for _, g := range public {
			if strings.Contains(g.Name, strings.TrimSuffix(q, "群")) || strings.Contains(q, g.Name) {
				target = &g
				break
			}
		}
	}
	if target == nil {
		if store.ParseCode(q) != "" {
			return errResult("房间号%s不存在，请再确认一下。", SpokenCode(store.ParseCode(q)))
		}
		return errResult("没有找到「%s」。私密群需要房间号才能加入。", q)
	}
	g, err := t.Store.JoinGroup(c.DeviceID, target.ID)
	if err != nil {
		return errResult("入群失败：%v", err)
	}
	st := t.API.Status(g)
	return Result{Text: fmt.Sprintf("已加入%s，%d人在线", g.Name, st.OnlineCount), Data: st}
}

func (t *tools) switchGroup(_ context.Context, c Caller, a Args) Result {
	if c.DeviceID == "" {
		return errNoDevice
	}
	delta := 0
	switch strings.ToLower(a.String("direction")) {
	case "next", "下一个":
		delta = 1
	case "previous", "prev", "上一个":
		delta = -1
	}
	q := a.String("query")
	if delta == 0 && q == "" {
		return errResult("要切到哪个群？")
	}
	if delta != 0 {
		q = ""
	}
	before, _ := t.Store.Device(c.DeviceID)
	g, candidates, err := t.API.Switch(c.DeviceID, "", q, delta)
	switch {
	case len(candidates) > 0:
		return Result{Text: fmt.Sprintf("找到多个群：%s，请问切到哪一个？", names(candidates)),
			Data: map[string]any{"candidates": candidates}}
	case errors.Is(err, store.ErrNotFound):
		if delta != 0 {
			return errResult("你还没有加入任何群。")
		}
		return errResult("你加入的群里没有「%s」。", q)
	case err != nil:
		return errResult("切换失败：%v", err)
	}
	st := t.API.Status(g)
	if g.ID == before.CurrentGroup {
		// No state change, so no channel event was pushed: confirm on device.
		t.Hub.SwitchNotify(c.DeviceID)
	}
	return Result{Text: t.statusText(st), Data: st}
}

func (t *tools) listMyGroups(_ context.Context, c Caller, _ Args) Result {
	if c.DeviceID == "" {
		return errNoDevice
	}
	ds, err := t.API.DeviceStatus(c.DeviceID)
	if err != nil || len(ds.List) == 0 {
		return Result{Text: "你还没有加入任何群。", Data: map[string]any{"groups": []any{}}}
	}
	var parts []string
	for i, g := range ds.List {
		p := fmt.Sprintf("%d、%s，%d人在线", i+1, g.Name, g.OnlineCount)
		if g.ID == ds.CurrentGroup {
			p += "（当前）"
		}
		parts = append(parts, p)
	}
	return Result{Text: fmt.Sprintf("你加入了%d个群：%s。", len(ds.List), strings.Join(parts, "；")),
		Data: map[string]any{"current_group": ds.CurrentGroup, "groups": ds.List}}
}

func (t *tools) groupStatus(_ context.Context, c Caller, a Args) Result {
	if c.DeviceID == "" {
		return errNoDevice
	}
	g, fail := t.target(c.DeviceID, a.String("query"))
	if fail != nil {
		return *fail
	}
	st := t.API.Status(g)
	var online []string
	speaker := ""
	for _, m := range st.Members {
		if m.Tuned {
			online = append(online, m.Name)
		}
		if m.Speaking {
			speaker = m.Name
		}
	}
	text := fmt.Sprintf("当前在%s，房间号%s，共%d人，%d人在线", g.Name, SpokenCode(g.Code), len(st.Members), st.OnlineCount)
	if len(online) > 0 && len(online) <= 8 {
		text += "：" + strings.Join(online, "、")
	}
	if speaker != "" {
		text += "。" + speaker + "正在说话"
	}
	d, _ := t.Store.Device(c.DeviceID)
	if d.IsMuted(g.ID, time.Now()) {
		text += "。这个群已静音"
	}
	if d.LeaderOnly[g.ID] {
		text += "。只听车队长"
	}
	return Result{Text: text + "。", Data: st}
}

func (t *tools) listNearby(_ context.Context, c Caller, a Args) Result {
	if c.DeviceID == "" {
		return errNoDevice
	}
	d, err := t.Store.Device(c.DeviceID)
	if err != nil || d.Location == nil {
		return errResult("还没有收到你的位置，暂时查不到附近的群。")
	}
	radius := a.Float("radius_km", t.NearbyRadiusKm)
	if radius <= 0 {
		radius = t.NearbyRadiusKm
	}
	groups := t.Store.NearbyGroups(*d.Location, radius)
	if len(groups) == 0 {
		return Result{Text: fmt.Sprintf("附近%g公里内没有群。可以说「建一个附近的群」。", radius), Data: map[string]any{"groups": []any{}}}
	}
	var parts []string
	for i, g := range groups {
		if i == 5 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s，%.1f公里，%d人", g.Name, g.DistanceKm, len(g.Members)))
	}
	return Result{Text: fmt.Sprintf("附近有%d个群：%s。说「加入」加群名就能进。", len(groups), strings.Join(parts, "；")),
		Data: map[string]any{"groups": groups}}
}

func (t *tools) listRoute(_ context.Context, c Caller, a Args) Result {
	route := a.String("route")
	groups := t.Store.RouteGroups(route)
	if len(groups) == 0 {
		return Result{Text: fmt.Sprintf("没有找到「%s」相关的路线群。可以说「建个跑%s的群」。", route, route), Data: map[string]any{"groups": []any{}}}
	}
	var parts []string
	for i, g := range groups {
		if i == 5 {
			break
		}
		parts = append(parts, fmt.Sprintf("%s（%s，%d人）", g.Name, g.Route, len(g.Members)))
	}
	return Result{Text: fmt.Sprintf("找到%d个路线群：%s。", len(groups), strings.Join(parts, "；")),
		Data: map[string]any{"groups": groups}}
}

func (t *tools) leaveGroup(_ context.Context, c Caller, a Args) Result {
	if c.DeviceID == "" {
		return errNoDevice
	}
	g, fail := t.target(c.DeviceID, a.String("query"))
	if fail != nil {
		return *fail
	}
	if err := t.Store.LeaveGroup(c.DeviceID, g.ID); err != nil {
		return errResult("退群失败：%v", err)
	}
	text := fmt.Sprintf("已退出%s。", g.Name)
	if d, _ := t.Store.Device(c.DeviceID); d.CurrentGroup != "" {
		if cur, err := t.Store.Group(d.CurrentGroup); err == nil {
			text += "现在在" + cur.Name + "。"
		}
	}
	return Result{Text: text}
}

func (t *tools) muteGroup(_ context.Context, c Caller, a Args) Result {
	if c.DeviceID == "" {
		return errNoDevice
	}
	g, fail := t.target(c.DeviceID, a.String("query"))
	if fail != nil {
		return *fail
	}
	minutes := a.Int("minutes", 60)
	until := api.MuteDeadline(time.Now(), minutes)
	if err := t.Store.MuteGroup(c.DeviceID, g.ID, until); err != nil {
		return errResult("操作失败：%v", err)
	}
	switch {
	case minutes == 0:
		return Result{Text: fmt.Sprintf("已取消%s的静音。", g.Name)}
	case minutes < 0:
		return Result{Text: fmt.Sprintf("%s已静音，说「取消静音」可恢复。", g.Name)}
	case minutes%60 == 0:
		return Result{Text: fmt.Sprintf("%s已静音%d小时。", g.Name, minutes/60)}
	}
	return Result{Text: fmt.Sprintf("%s已静音%d分钟。", g.Name, minutes)}
}

func (t *tools) leaderOnly(_ context.Context, c Caller, a Args) Result {
	if c.DeviceID == "" {
		return errNoDevice
	}
	g, fail := t.target(c.DeviceID, a.String("query"))
	if fail != nil {
		return *fail
	}
	on := a.Bool("on", true)
	if err := t.Store.SetLeaderOnly(c.DeviceID, g.ID, on); err != nil {
		return errResult("操作失败：%v", err)
	}
	if on {
		return Result{Text: fmt.Sprintf("好的，%s只播放车队长的语音。", g.Name)}
	}
	return Result{Text: fmt.Sprintf("已恢复收听%s所有人的语音。", g.Name)}
}

func (t *tools) replayLast(_ context.Context, c Caller, a Args) Result {
	if c.DeviceID == "" {
		return errNoDevice
	}
	g, fail := t.target(c.DeviceID, a.String("query"))
	if fail != nil {
		return *fail
	}
	if !t.Hub.Online(c.DeviceID) {
		return errResult("对讲机不在线，无法回放。")
	}
	n := min(max(a.Int("n", 1), 1), 10)
	recs, err := t.Archive.ReplayLast(c.DeviceID, g.ID, "", n)
	switch {
	case errors.Is(err, voice.ErrOffline):
		return errResult("对讲机不在线，无法回放。")
	case err != nil:
		return errResult("%s最近没有可以回放的语音。", g.Name)
	}
	who := make([]string, len(recs))
	for i, r := range recs {
		who[i] = r.SpeakerName
	}
	return Result{Text: fmt.Sprintf("开始回放%d条，来自%s。", len(recs), strings.Join(who, "、")), Data: map[string]any{"recordings": recs}}
}

func (t *tools) whoSpoke(_ context.Context, c Caller, a Args) Result {
	if c.DeviceID == "" {
		return errNoDevice
	}
	g, fail := t.target(c.DeviceID, a.String("query"))
	if fail != nil {
		return *fail
	}
	n := min(max(a.Int("n", 3), 1), 10)
	recs := t.Archive.Archive.Recent(g.ID, n)
	if len(recs) == 0 {
		return Result{Text: fmt.Sprintf("%s最近没人说话。", g.Name)}
	}
	now := time.Now()
	var parts []string
	for _, r := range recs {
		p := fmt.Sprintf("%s%s说了%d秒", r.SpeakerName, ago(r.StartedAt, now), (r.DurationMs+500)/1000)
		if r.Transcript != "" {
			p += "：「" + r.Transcript + "」"
		}
		parts = append(parts, p)
	}
	return Result{Text: strings.Join(parts, "；") + "。", Data: map[string]any{"recordings": recs}}
}

func (t *tools) sendText(_ context.Context, c Caller, a Args) Result {
	if c.DeviceID == "" {
		return errNoDevice
	}
	text := strings.TrimSpace(a.String("text"))
	if text == "" {
		return errResult("要发什么内容？")
	}
	g, fail := t.target(c.DeviceID, a.String("query"))
	if fail != nil {
		return *fail
	}
	if !g.HasMember(c.DeviceID) {
		return errResult("你不在%s里。", g.Name)
	}
	d, _ := t.Store.Device(c.DeviceID)
	t.Hub.NotifyGroup(g.ID, "info", fmt.Sprintf("%s留言：%s", d.Name, text))
	t.Store.Touch(g.ID)
	st := t.API.Status(g)
	return Result{Text: fmt.Sprintf("已发到%s，%d人在线收到。", g.Name, st.OnlineCount)}
}

func (t *tools) shareGroup(_ context.Context, c Caller, a Args) Result {
	if c.DeviceID == "" {
		return errNoDevice
	}
	g, fail := t.target(c.DeviceID, a.String("query"))
	if fail != nil {
		return *fail
	}
	url := t.API.ShareURL(g.Code)
	return Result{
		Text: fmt.Sprintf("%s的房间号是%s。对方说「加入房间%s」，或者扫码打开 %s 就能进群。", g.Name, SpokenCode(g.Code), SpokenCode(g.Code), url),
		Data: map[string]any{"code": g.Code, "code_spoken": SpokenCode(g.Code), "share_url": url, "qr_url": strings.TrimSuffix(url, "/join/"+g.Code) + "/api/groups/" + g.ID + "/qr.png"},
	}
}

func (t *tools) dissolveGroup(_ context.Context, c Caller, a Args) Result {
	if c.DeviceID == "" {
		return errNoDevice
	}
	g, fail := t.target(c.DeviceID, a.String("query"))
	if fail != nil {
		return *fail
	}
	if !g.IsLeader(c.DeviceID) {
		return errResult("只有车队长可以解散%s。", g.Name)
	}
	if err := t.Store.DissolveGroup(g.ID); err != nil {
		return errResult("解散失败：%v", err)
	}
	return Result{Text: fmt.Sprintf("%s已解散。", g.Name)}
}

func (t *tools) summarize(ctx context.Context, c Caller, a Args) Result {
	if c.DeviceID == "" {
		return errNoDevice
	}
	if t.AI == nil {
		return errResult("摘要功能未启用。")
	}
	g, fail := t.target(c.DeviceID, a.String("query"))
	if fail != nil {
		return *fail
	}
	minutes := min(max(a.Int("minutes", 30), 1), 24*60)
	sum, err := t.AI.Summarize(ctx, c.DeviceID, g.ID, time.Now().Add(-time.Duration(minutes)*time.Minute))
	if err != nil {
		return errResult("摘要失败：%v", err)
	}
	return Result{Text: sum.Text, Data: sum}
}

func (t *tools) pairNearby(ctx context.Context, c Caller, _ Args) Result {
	if c.DeviceID == "" {
		return errNoDevice
	}
	if t.Social == nil {
		return errResult("配对功能未启用。")
	}
	g, err := t.Social.Pair(ctx, c.DeviceID)
	if errors.Is(err, social.ErrNoPartner) {
		return errResult("没有找到同时配对的设备，请两台对讲机一起说「配对」。")
	}
	if err != nil {
		return errResult("配对失败：%v", err)
	}
	return Result{Text: fmt.Sprintf("配对成功，已进入临时对讲%s，房间号%s。", g.Name, SpokenCode(g.Code)), Data: g}
}

func (t *tools) inviteMember(_ context.Context, c Caller, a Args) Result {
	if c.DeviceID == "" {
		return errNoDevice
	}
	if t.Social == nil {
		return errResult("邀请功能未启用。")
	}
	g, fail := t.target(c.DeviceID, a.String("query"))
	if fail != nil {
		return *fail
	}
	who := a.String("target")
	targets, err := t.Social.Invite(c.DeviceID, g.ID, who)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return errResult("没有找到「%s」的对讲机。可以把房间号%s告诉对方。", who, SpokenCode(g.Code))
	case errors.Is(err, store.ErrNotMember):
		return errResult("你不在%s里，不能邀请别人。", g.Name)
	case err != nil:
		return errResult("邀请失败：%v", err)
	}
	names := make([]string, len(targets))
	for i, d := range targets {
		names[i] = d.Name
	}
	return Result{Text: fmt.Sprintf("已邀请%s加入%s，对方说「接受邀请」就能进群。", strings.Join(names, "、"), g.Name),
		Data: map[string]any{"invited": targets}}
}

func (t *tools) acceptInvite(_ context.Context, c Caller, _ Args) Result {
	if c.DeviceID == "" {
		return errNoDevice
	}
	if t.Social == nil {
		return errResult("邀请功能未启用。")
	}
	g, inv, err := t.Social.Accept(c.DeviceID, "")
	if err != nil {
		return errResult("你没有待处理的邀请。")
	}
	st := t.API.Status(g)
	return Result{Text: fmt.Sprintf("已接受%s的邀请，加入%s，%d人在线。", inv.FromName, g.Name, st.OnlineCount), Data: st}
}
