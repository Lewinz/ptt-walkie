package store

import "time"

// TalkPolicy controls who may take the floor in a group.
type TalkPolicy string

const (
	TalkAll     TalkPolicy = "all"     // everyone may talk (default)
	TalkLeaders TalkPolicy = "leaders" // broadcast mode: only leaders talk, members listen
)

// Template presets group behaviour for a scenario (requirements: 场景扩展).
type Template struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Type        GroupType     `json:"type"`
	TalkPolicy  TalkPolicy    `json:"talk_policy"`
	AlertWords  []string      `json:"alert_words,omitempty"`  // in addition to the global list
	IdleTimeout time.Duration `json:"idle_timeout,omitempty"` // temp groups only; 0 = server default
	// Greeting is spoken to a device when it joins; %s is the group name.
	Greeting string `json:"greeting"`
}

var templates = []Template{
	{ID: "driver", Name: "车队", Description: "司机车队、货运、网约车：人人可讲，车队长可插话",
		Type: GroupFixed, TalkPolicy: TalkAll,
		AlertWords: []string{"查车", "限行", "改道", "爆胎"},
		Greeting:   "欢迎加入%s，按住按键说话，一路平安"},
	{ID: "cycling", Name: "骑行", Description: "骑行队伍：临时群，掉队、爆胎预警",
		Type: GroupTemp, TalkPolicy: TalkAll, IdleTimeout: 6 * time.Hour,
		AlertWords: []string{"掉队", "爆胎", "摔车", "等一下"},
		Greeting:   "欢迎加入%s，注意骑行安全"},
	{ID: "hiking", Name: "登山", Description: "登山徒步：迷路、受伤预警，长时间保留",
		Type: GroupTemp, TalkPolicy: TalkAll, IdleTimeout: 24 * time.Hour,
		AlertWords: []string{"迷路", "受伤", "掉队", "下雨", "天黑"},
		Greeting:   "欢迎加入%s，跟紧队伍，有情况马上说"},
	{ID: "guide", Name: "导游", Description: "景区导游 / 讲解：只有导游能讲，游客只听",
		Type: GroupTemp, TalkPolicy: TalkLeaders, IdleTimeout: 12 * time.Hour,
		AlertWords: []string{"集合", "走散"},
		Greeting:   "欢迎加入%s，请留意导游的讲解和集合通知"},
	{ID: "worksite", Name: "工地班组", Description: "工地 / 仓库班组：安全事故词预警",
		Type: GroupFixed, TalkPolicy: TalkAll,
		AlertWords: []string{"受伤", "塌方", "漏电", "起火", "停电", "撤离"},
		Greeting:   "欢迎加入%s，注意作业安全"},
	{ID: "family", Name: "家庭", Description: "家人（老人 / 孩子的设备）：长期保留，求助词预警",
		Type: GroupFixed, TalkPolicy: TalkAll,
		AlertWords: []string{"救命", "摔倒", "不舒服", "迷路", "快来"},
		Greeting:   "欢迎加入%s，按住按键就能和家人说话"},
}

// Templates lists the built-in scenario templates.
func Templates() []Template { return templates }

// TemplateByID returns the template, or ok=false.
func TemplateByID(id string) (Template, bool) {
	for _, t := range templates {
		if t.ID == id {
			return t, true
		}
	}
	return Template{}, false
}

// TemplateFor returns the group's template, falling back to "driver".
func TemplateFor(g GroupView) Template {
	if t, ok := TemplateByID(g.Template); ok {
		return t
	}
	return templates[0]
}
