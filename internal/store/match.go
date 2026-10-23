package store

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// Filler words that drivers naturally add when naming a group by voice
// ("切到车队那个群", "去老乡频道").
var fillers = []string{
	"切换到", "切换", "切到", "换到", "转到", "进入", "加入", "去", "到",
	"那个", "这个", "我们的", "我们", "咱们的", "咱们", "一下",
	"群聊", "群组", "群", "频道", "房间", "对讲", "的",
}

func normalizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, f := range fillers {
		s = strings.ReplaceAll(s, f, "")
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsPunct(r) {
			return -1
		}
		return r
	}, s)
}

// matchScore rates how well a spoken query matches a group name, 0..100.
func matchScore(query, name string) float64 {
	q, n := normalizeName(query), normalizeName(name)
	if q == "" || n == "" {
		return 0
	}
	if q == n {
		return 100
	}
	if strings.Contains(n, q) || strings.Contains(q, n) {
		short, long := len([]rune(q)), len([]rune(n))
		if short > long {
			short, long = long, short
		}
		return 70 + 20*float64(short)/float64(long)
	}
	// Character overlap: robust to ASR mixing up word order / particles.
	nr := map[rune]int{}
	for _, r := range n {
		nr[r]++
	}
	common := 0
	qr := []rune(q)
	for _, r := range qr {
		if nr[r] > 0 {
			nr[r]--
			common++
		}
	}
	denom := math.Max(float64(len(qr)), float64(len([]rune(n))))
	return 60 * float64(common) / denom
}

type scored struct {
	g     *Group
	score float64
}

func rankGroups(query string, groups []*Group) []scored {
	var out []scored
	for _, g := range groups {
		if s := matchScore(query, g.Name); s >= 30 {
			out = append(out, scored{g, s})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	return out
}
