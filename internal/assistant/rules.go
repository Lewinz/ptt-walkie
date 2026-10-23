package assistant

import (
	"regexp"
	"strconv"
	"strings"
)

// Intent is a parsed voice command mapped onto an MCP tool.
type Intent struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
}

type rule struct {
	re    *regexp.Regexp
	build func(m []string, text string) Intent
}

func re(s string) *regexp.Regexp { return regexp.MustCompile(s) }

func intent(tool string, kv ...any) Intent {
	args := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		if v, ok := kv[i+1].(string); ok && v == "" {
			continue
		}
		args[kv[i].(string)] = kv[i+1]
	}
	return Intent{Tool: tool, Args: args}
}

// groupRef cleans a spoken group reference: "大家" / "群里" mean the
// current group.
func groupRef(s string) string {
	s = strings.Trim(s, "的里 ")
	switch s {
	case "", "大家", "群里", "所有人", "这个群", "当前群", "这个", "本群":
		return ""
	}
	return s
}

func guessTemplate(name string) string {
	for _, kw := range []struct{ words, id string }{
		{"骑行|单车|车友会", "cycling"},
		{"登山|徒步|爬山|户外", "hiking"},
		{"导游|旅游团|旅行团|讲解|景区", "guide"},
		{"工地|班组|仓库|车间|施工", "worksite"},
		{"家人|家庭|一家|爸妈|全家", "family"},
	} {
		for w := range strings.SplitSeq(kw.words, "|") {
			if strings.Contains(name, w) {
				return kw.id
			}
		}
	}
	return ""
}

var cnDigits = map[rune]int{'零': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}

// cnNumber parses "30", "三十", "两", "十五", "一百二十" (up to 999).
func cnNumber(s string) (int, bool) {
	if n, err := strconv.Atoi(s); err == nil {
		return n, true
	}
	total, cur, seen := 0, 0, false
	for _, r := range s {
		switch r {
		case '百':
			total += max(cur, 1) * 100
			cur = 0
		case '十':
			total += max(cur, 1) * 10
			cur = 0
		default:
			d, ok := cnDigits[r]
			if !ok {
				return 0, false
			}
			cur = d
		}
		seen = true
	}
	return total + cur, seen
}

var durationRe = re(`(半|[0-9]+|[零一二两三四五六七八九十百]+)个?(小时|钟头|分钟|分)`)

// parseMinutes extracts "一小时" / "半小时" / "30分钟"; def when absent.
func parseMinutes(text string, def int) int {
	m := durationRe.FindStringSubmatch(text)
	if m == nil {
		return def
	}
	if m[1] == "半" {
		if m[2] == "小时" || m[2] == "钟头" {
			return 30
		}
		return def
	}
	n, ok := cnNumber(m[1])
	if !ok {
		return def
	}
	if m[2] == "小时" || m[2] == "钟头" {
		return n * 60
	}
	return n
}

var countRe = re(`([0-9]+|[一二两三四五六七八九十]+)(条|句|段|个)`)

func parseCount(text string, def int) int {
	if m := countRe.FindStringSubmatch(text); m != nil {
		if n, ok := cnNumber(m[1]); ok && n > 0 {
			return n
		}
	}
	return def
}

const sep = `[:：，,。 ]*`

var rules = []rule{
	{re(`(取消|解除|关闭|关掉)静音`), func(m []string, _ string) Intent {
		return intent("mute_group", "minutes", 0)
	}},
	{re(`静音|别吵|先安静|屏蔽`), func(m []string, t string) Intent {
		return intent("mute_group", "minutes", parseMinutes(t, 60))
	}},
	{re(`只听(车)?队长|只听领队|只听导游`), func(m []string, _ string) Intent {
		return intent("listen_leader_only", "on", true)
	}},
	{re(`(恢复|听)所有人|取消只听`), func(m []string, _ string) Intent {
		return intent("listen_leader_only", "on", false)
	}},
	{re(`^(?:帮我)?(?:给|向)(.+?)(?:发|说|留言|带句话)` + sep + `(.+)$`), func(m []string, _ string) Intent {
		return intent("send_text_message", "query", groupRef(m[1]), "text", m[2])
	}},
	{re(`^(?:告诉|通知)(大家|群里|所有人)` + sep + `(.+)$`), func(m []string, _ string) Intent {
		return intent("send_text_message", "text", m[2])
	}},
	{re(`(?:建|创建|新建|拉)一?个?(?:附近|周边)的?(?:临时)?群(?:叫做?|名叫)?` + sep + `(.*)$`), func(m []string, _ string) Intent {
		name := strings.TrimSpace(m[1])
		if name == "" {
			name = "附近临时群"
		}
		return intent("create_group", "name", name, "visibility", "nearby", "type", "temp", "template", guessTemplate(name))
	}},
	{re(`(?:建|创建|新建|拉)一?个?(?:跑|走|去)(.+?)(?:线|路线|方向)?的群`), func(m []string, _ string) Intent {
		return intent("create_group", "name", m[1]+"车队", "visibility", "route", "route", m[1])
	}},
	{re(`(?:建|创建|新建|拉)一?个?(?:群|频道|对讲群)?(?:叫做|叫|名叫|名字叫|名字是)?` + sep + `(.+)$`), func(m []string, _ string) Intent {
		name := strings.TrimSpace(m[1])
		return intent("create_group", "name", name, "template", guessTemplate(name))
	}},
	{re(`解散(.*)`), func(m []string, _ string) Intent {
		return intent("dissolve_group", "query", groupRef(m[1]))
	}},
	{re(`退出(?:群)?(.*)|^退群$`), func(m []string, _ string) Intent {
		return intent("leave_group", "query", groupRef(m[1]))
	}},
	{re(`错过|说了(什么|啥)|有(什么|啥)(新)?消息|总结一下|汇总`), func(m []string, t string) Intent {
		return intent("summarize_missed", "minutes", parseMinutes(t, 30))
	}},
	{re(`再放|重放|回放|再听|没听清|重播`), func(m []string, t string) Intent {
		return intent("replay_last", "n", parseCount(t, 1))
	}},
	{re(`(正在|现在)谁在?说话|谁在说话|谁在讲`), func(m []string, _ string) Intent {
		return intent("get_group_status")
	}},
	{re(`谁说的|谁说话了|谁讲的|谁在呼叫`), func(m []string, t string) Intent {
		return intent("who_spoke_last", "n", parseCount(t, 3))
	}},
	{re(`(我|现在)在哪个(群|频道)|当前(群|频道)|群里有谁|谁在线|几个人在线|多少人在线|有几个人`), func(m []string, _ string) Intent {
		return intent("get_group_status")
	}},
	{re(`(我|我的)(有|加了|加入了|都有)?(哪些|几个|什么)群|我的群`), func(m []string, _ string) Intent {
		return intent("list_my_groups")
	}},
	{re(`房间号|群号|怎么(让|拉|邀请).*(进|加)|分享(这个)?群|邀请`), func(m []string, _ string) Intent {
		return intent("share_group")
	}},
	{re(`有没有(?:跑|走|去)?(.+?)(?:线|路线|方向)?的群`), func(m []string, _ string) Intent {
		return intent("list_route_groups", "route", m[1])
	}},
	{re(`附近.*群|周边.*群|这儿有群`), func(m []string, _ string) Intent {
		return intent("list_nearby_groups")
	}},
	{re(`下一个|下个频道|下一频道`), func(m []string, _ string) Intent {
		return intent("switch_group", "direction", "next")
	}},
	{re(`上一个|上个频道|上一频道`), func(m []string, _ string) Intent {
		return intent("switch_group", "direction", "previous")
	}},
	{re(`(?:加入|进入|进到|加)(?:一下)?(?:房间|频道|群)?号?` + sep + `(.+)$`), func(m []string, _ string) Intent {
		return intent("join_group", "query", m[1])
	}},
	{re(`(?:切换到|切到|切回|换到|转到|回到|切换|去)` + sep + `(.+)$`), func(m []string, _ string) Intent {
		return intent("switch_group", "query", m[1])
	}},
}

var trailing = re(`[。！？!?，,~～ ]+$|(吧|啊|呀|哈|一下|好吗|可以吗)+$`)

// ParseIntent maps a spoken sentence onto a tool call, or ok=false.
func ParseIntent(text string) (Intent, bool) {
	t := strings.TrimSpace(text)
	t = strings.TrimPrefix(t, "小智")
	t = strings.TrimPrefix(t, "灵矽")
	t = strings.TrimLeft(t, "，, ")
	for range 3 {
		t = trailing.ReplaceAllString(t, "")
	}
	if t == "" {
		return Intent{}, false
	}
	for _, r := range rules {
		if m := r.re.FindStringSubmatch(t); m != nil {
			return r.build(m, t), true
		}
	}
	return Intent{}, false
}
