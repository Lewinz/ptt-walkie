package main

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/plant-squad/ptt-walkie/internal/config"
	"github.com/plant-squad/ptt-walkie/internal/sim"
	"github.com/plant-squad/ptt-walkie/internal/store"
)

const echoBotID = "echo-bot"

// seedDemo creates showcase groups on an empty store and returns the echo
// group's room code.
func seedDemo(st *store.Store) string {
	for _, g := range st.AllGroups() {
		if g.OwnerID == echoBotID {
			return g.Code // already seeded
		}
	}
	st.EnsureDevice(echoBotID, "回声测试")
	echo, _ := st.CreateGroup(echoBotID, store.CreateGroupInput{Name: "回声测试"})

	for _, d := range [][2]string{{"car-001", "王队长"}, {"car-002", "沪A12345"}, {"car-003", "苏E88888"}, {"car-004", "皖B66666"}} {
		st.EnsureDevice(d[0], d[1])
	}
	fleet, _ := st.CreateGroup("car-001", store.CreateGroupInput{Name: "京沪线车队", Template: "driver"})
	for _, id := range []string{"car-002", "car-003", "car-004"} {
		st.JoinGroup(id, fleet.ID)
	}
	home, _ := st.CreateGroup("car-003", store.CreateGroupInput{Name: "河南老乡群"})
	st.JoinGroup("car-004", home.ID)
	st.CreateGroup("car-004", store.CreateGroupInput{Name: "沪蓉冷链车队", Visibility: store.VisRoute, Route: "上海-成都 G42 沪蓉高速"})
	yangcheng := store.GeoPoint{Lat: 31.4296, Lng: 120.7907} // 京沪高速 阳澄湖服务区
	st.CreateGroup("car-002", store.CreateGroupInput{Name: "阳澄湖服务区拼车", Type: store.GroupTemp, Visibility: store.VisNearby, Location: &yangcheng})
	st.CreateGroup("car-001", store.CreateGroupInput{Name: "故宫一日游", Template: "guide"})
	sos, _ := st.CreateGroup("car-001", store.CreateGroupInput{Name: "全国紧急频道", Type: store.GroupEmergency})
	for _, id := range []string{"car-002", "car-003", "car-004"} {
		st.AddMember(id, sos.ID)
	}
	slog.Info("demo data seeded", "echo_code", echo.Code, "fleet_code", fleet.Code)
	return echo.Code
}

// localURL is how in-process bots reach this server.
func localURL(cfg config.Config) string {
	if cfg.TLSCert != "" {
		return cfg.BaseURL
	}
	addr := cfg.Addr
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	return "http://" + addr
}

// runEchoBot keeps the radio-check bot connected for the server lifetime.
func runEchoBot(ctx context.Context, server, code string) {
	for ctx.Err() == nil {
		if err := sim.RunEcho(ctx, server, echoBotID, "回声测试", code); err != nil && ctx.Err() == nil {
			slog.Debug("echo bot", "err", err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
}
