package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store is the in-memory, snapshot-persisted source of truth for groups and
// devices. All methods are safe for concurrent use and return copies.
type Store struct {
	mu      sync.RWMutex
	groups  map[string]*Group
	byCode  map[string]string // code → group id
	devices map[string]*Device

	path  string
	dirty bool
	now   func() time.Time

	subMu sync.RWMutex
	subs  []func(Event)
}

type snapshot struct {
	Groups  map[string]*Group  `json:"groups"`
	Devices map[string]*Device `json:"devices"`
}

// New creates a store persisted at path ("" for memory only) and loads any
// existing snapshot.
func New(path string) (*Store, error) {
	s := &Store{
		groups:  map[string]*Group{},
		byCode:  map[string]string{},
		devices: map[string]*Device{},
		path:    path,
		now:     time.Now,
	}
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var snap snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for id, g := range snap.Groups {
		s.groups[id] = g
		s.byCode[g.Code] = id
	}
	maps.Copy(s.devices, snap.Devices)
	return s, nil
}

// Subscribe registers a listener invoked synchronously after each change.
func (s *Store) Subscribe(fn func(Event)) {
	s.subMu.Lock()
	s.subs = append(s.subs, fn)
	s.subMu.Unlock()
}

func (s *Store) emit(evs ...Event) {
	s.subMu.RLock()
	subs := slices.Clone(s.subs)
	s.subMu.RUnlock()
	for _, ev := range evs {
		for _, fn := range subs {
			fn(ev)
		}
	}
}

// Run periodically flushes dirty state to disk until ctx is cancelled.
func (s *Store) Run(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := s.Flush(); err != nil {
				slog.Error("flush state", "err", err)
			}
			return
		case <-t.C:
			if err := s.Flush(); err != nil {
				slog.Error("flush state", "err", err)
			}
		}
	}
}

// Flush writes the snapshot atomically if anything changed.
func (s *Store) Flush() error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	data, err := json.MarshalIndent(snapshot{Groups: s.groups, Devices: s.devices}, "", "  ")
	s.dirty = false
	s.mu.Unlock()
	if err == nil {
		err = writeAtomic(s.path, data)
	}
	if err != nil {
		// Keep the state dirty so the next tick retries.
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
	}
	return err
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---- devices ----

// EnsureDevice returns the device, registering it on first sight. A non-empty
// name updates the display name.
func (s *Store) EnsureDevice(id, name string) (DeviceView, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return DeviceView{}, fmt.Errorf("%w: empty device id", ErrInvalidInput)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.ensureDevice(id)
	if name = strings.TrimSpace(name); name != "" && d.Name != name {
		d.Name = name
		s.dirty = true
	}
	return d.view(), nil
}

func (s *Store) ensureDevice(id string) *Device {
	d, ok := s.devices[id]
	if !ok {
		d = &Device{ID: id, Name: defaultDeviceName(id), CreatedAt: s.now()}
		s.devices[id] = d
		s.dirty = true
	}
	return d
}

func defaultDeviceName(id string) string {
	r := []rune(id)
	if len(r) > 4 {
		r = r[len(r)-4:]
	}
	return "设备" + string(r)
}

func (s *Store) Device(id string) (DeviceView, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.devices[id]
	if !ok {
		return DeviceView{}, ErrNotFound
	}
	return d.view(), nil
}

func (s *Store) UpdateLocation(deviceID string, p GeoPoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.ensureDevice(deviceID)
	d.Location = &p
	d.LocationAt = s.now()
	s.dirty = true
	return nil
}

// FindDevices matches devices by exact id or by display name (spaces and
// case ignored), e.g. a licence plate "沪A 12345".
func (s *Store) FindDevices(q string) []DeviceView {
	norm := func(x string) string {
		return strings.ToUpper(strings.Join(strings.Fields(x), ""))
	}
	nq := norm(q)
	if nq == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if d, ok := s.devices[q]; ok {
		return []DeviceView{d.view()}
	}
	var exact, partial []DeviceView
	for _, d := range s.devices {
		switch n := norm(d.Name); {
		case n == nq:
			exact = append(exact, d.view())
		case len([]rune(nq)) >= 3 && strings.Contains(n, nq):
			partial = append(partial, d.view())
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return partial
}

// ---- groups ----

type CreateGroupInput struct {
	Name       string
	Type       GroupType
	Visibility Visibility
	Route      string
	Location   *GeoPoint
	Template   string     // scenario template id; fills Type / TalkPolicy defaults
	TalkPolicy TalkPolicy // overrides the template
}

// CreateGroup creates a group owned by ownerID, who joins it as leader and
// switches to it.
func (s *Store) CreateGroup(ownerID string, in CreateGroupInput) (GroupView, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return GroupView{}, fmt.Errorf("%w: group name required", ErrInvalidInput)
	}
	if in.Template != "" {
		t, ok := TemplateByID(in.Template)
		if !ok {
			return GroupView{}, fmt.Errorf("%w: unknown template %q", ErrInvalidInput, in.Template)
		}
		if in.Type == "" {
			in.Type = t.Type
		}
		if in.TalkPolicy == "" {
			in.TalkPolicy = t.TalkPolicy
		}
	}
	if in.Type == "" {
		in.Type = GroupFixed
	}
	if in.TalkPolicy == "" {
		in.TalkPolicy = TalkAll
	}
	if in.TalkPolicy != TalkAll && in.TalkPolicy != TalkLeaders {
		return GroupView{}, fmt.Errorf("%w: unknown talk policy %q", ErrInvalidInput, in.TalkPolicy)
	}
	if in.Visibility == "" {
		in.Visibility = VisCode
	}
	switch in.Type {
	case GroupTemp, GroupFixed, GroupEmergency:
	default:
		return GroupView{}, fmt.Errorf("%w: unknown group type %q", ErrInvalidInput, in.Type)
	}
	switch in.Visibility {
	case VisCode, VisRoute:
	case VisNearby:
		if in.Location == nil {
			return GroupView{}, fmt.Errorf("%w: nearby group needs a location", ErrInvalidInput)
		}
	default:
		return GroupView{}, fmt.Errorf("%w: unknown visibility %q", ErrInvalidInput, in.Visibility)
	}

	s.mu.Lock()
	now := s.now()
	g := &Group{
		ID:           randomID("g_"),
		Code:         s.newCode(),
		Name:         in.Name,
		Type:         in.Type,
		Visibility:   in.Visibility,
		OwnerID:      ownerID,
		Template:     in.Template,
		TalkPolicy:   in.TalkPolicy,
		Route:        strings.TrimSpace(in.Route),
		Location:     in.Location,
		Members:      map[string]*Member{},
		CreatedAt:    now,
		LastActiveAt: now,
	}
	s.groups[g.ID] = g
	s.byCode[g.Code] = g.ID
	s.dirty = true
	var evs []Event
	if ownerID != "" {
		evs = s.join(ownerID, g, RoleLeader)
	}
	v := g.view()
	s.mu.Unlock()
	s.emit(evs...)
	return v, nil
}

func (s *Store) newCode() string {
	length := 4
	// 7 * 8^3 = 3584 four-digit codes; grow once half are taken.
	if len(s.byCode) > 1700 {
		length = 5
	}
	for {
		c := randomCode(length)
		if _, taken := s.byCode[c]; !taken {
			return c
		}
	}
}

func (s *Store) Group(id string) (GroupView, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.groups[id]
	if !ok {
		return GroupView{}, ErrNotFound
	}
	return g.view(), nil
}

func (s *Store) GroupByCode(code string) (GroupView, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byCode[code]
	if !ok {
		return GroupView{}, ErrNotFound
	}
	return s.groups[id].view(), nil
}

func (s *Store) AllGroups() []GroupView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]GroupView, 0, len(s.groups))
	for _, g := range s.groups {
		out = append(out, g.view())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// DeviceGroups returns the device's groups in join order.
func (s *Store) DeviceGroups(deviceID string) []GroupView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.devices[deviceID]
	if !ok {
		return nil
	}
	out := make([]GroupView, 0, len(d.Groups))
	for _, id := range d.Groups {
		if g, ok := s.groups[id]; ok {
			out = append(out, g.view())
		}
	}
	return out
}

// JoinGroup adds the device to the group and makes it the current channel.
func (s *Store) JoinGroup(deviceID, groupID string) (GroupView, error) {
	s.mu.Lock()
	g, ok := s.groups[groupID]
	if !ok {
		s.mu.Unlock()
		return GroupView{}, ErrNotFound
	}
	evs := s.join(deviceID, g, RoleMember)
	v := g.view()
	s.mu.Unlock()
	s.emit(evs...)
	return v, nil
}

// AddMember adds the device to the group without changing its current
// channel, unless it has none yet.
func (s *Store) AddMember(deviceID, groupID string) (GroupView, error) {
	s.mu.Lock()
	g, ok := s.groups[groupID]
	if !ok {
		s.mu.Unlock()
		return GroupView{}, ErrNotFound
	}
	d := s.ensureDevice(deviceID)
	var evs []Event
	if d.CurrentGroup == "" {
		evs = s.join(deviceID, g, RoleMember)
	} else if _, ok := g.Members[deviceID]; !ok {
		g.Members[deviceID] = &Member{DeviceID: deviceID, Role: RoleMember, JoinedAt: s.now()}
		d.Groups = append(d.Groups, g.ID)
		s.dirty = true
		evs = append(evs, Event{Type: EvJoined, DeviceID: deviceID, GroupID: g.ID})
	}
	v := g.view()
	s.mu.Unlock()
	s.emit(evs...)
	return v, nil
}

func (s *Store) join(deviceID string, g *Group, role Role) []Event {
	d := s.ensureDevice(deviceID)
	var evs []Event
	if _, ok := g.Members[deviceID]; !ok {
		g.Members[deviceID] = &Member{DeviceID: deviceID, Role: role, JoinedAt: s.now()}
		d.Groups = append(d.Groups, g.ID)
		evs = append(evs, Event{Type: EvJoined, DeviceID: deviceID, GroupID: g.ID})
	}
	if d.CurrentGroup != g.ID {
		d.CurrentGroup = g.ID
		evs = append(evs, Event{Type: EvSwitched, DeviceID: deviceID, GroupID: g.ID})
	}
	s.dirty = true
	return evs
}

// SwitchGroup makes groupID the device's current channel. The device must
// already be a member.
func (s *Store) SwitchGroup(deviceID, groupID string) (GroupView, error) {
	s.mu.Lock()
	g, ok := s.groups[groupID]
	if !ok {
		s.mu.Unlock()
		return GroupView{}, ErrNotFound
	}
	if _, ok := g.Members[deviceID]; !ok {
		s.mu.Unlock()
		return GroupView{}, ErrNotMember
	}
	evs := s.join(deviceID, g, RoleMember)
	v := g.view()
	s.mu.Unlock()
	s.emit(evs...)
	return v, nil
}

// CycleGroup moves the device delta steps through its group list
// ("下一个频道" = +1, "上一个" = -1), wrapping around.
func (s *Store) CycleGroup(deviceID string, delta int) (GroupView, error) {
	s.mu.RLock()
	d, ok := s.devices[deviceID]
	if !ok || len(d.Groups) == 0 {
		s.mu.RUnlock()
		return GroupView{}, ErrNotFound
	}
	idx := slices.Index(d.Groups, d.CurrentGroup)
	if idx < 0 {
		idx = 0
		if delta > 0 {
			delta--
		}
	}
	n := len(d.Groups)
	target := d.Groups[((idx+delta)%n+n)%n]
	s.mu.RUnlock()
	return s.SwitchGroup(deviceID, target)
}

// LeaveGroup removes the device from the group. If it was the current
// channel, the device falls back to its most recently joined other group.
func (s *Store) LeaveGroup(deviceID, groupID string) error {
	s.mu.Lock()
	g, ok := s.groups[groupID]
	if !ok {
		s.mu.Unlock()
		return ErrNotFound
	}
	if _, ok := g.Members[deviceID]; !ok {
		s.mu.Unlock()
		return ErrNotMember
	}
	delete(g.Members, deviceID)
	d := s.ensureDevice(deviceID)
	d.Groups = slices.DeleteFunc(d.Groups, func(id string) bool { return id == groupID })
	delete(d.MutedUntil, groupID)
	delete(d.LeaderOnly, groupID)
	evs := []Event{{Type: EvLeft, DeviceID: deviceID, GroupID: groupID}}
	if d.CurrentGroup == groupID {
		d.CurrentGroup = ""
		if n := len(d.Groups); n > 0 {
			d.CurrentGroup = d.Groups[n-1]
		}
		evs = append(evs, Event{Type: EvSwitched, DeviceID: deviceID, GroupID: d.CurrentGroup})
	}
	// Hand leadership over when the owner leaves.
	if g.OwnerID == deviceID {
		g.OwnerID = ""
		var oldest *Member
		for _, m := range g.Members {
			if oldest == nil || m.JoinedAt.Before(oldest.JoinedAt) {
				oldest = m
			}
		}
		if oldest != nil {
			oldest.Role = RoleLeader
			g.OwnerID = oldest.DeviceID
		}
	}
	s.dirty = true
	s.mu.Unlock()
	s.emit(evs...)
	return nil
}

// DissolveGroup deletes the group and detaches all members.
func (s *Store) DissolveGroup(groupID string) error {
	s.mu.Lock()
	evs, ok := s.dissolve(groupID)
	s.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	s.emit(evs...)
	return nil
}

func (s *Store) dissolve(groupID string) ([]Event, bool) {
	g, ok := s.groups[groupID]
	if !ok {
		return nil, false
	}
	var members []string
	var evs []Event
	for id := range g.Members {
		members = append(members, id)
		d := s.devices[id]
		if d == nil {
			continue
		}
		d.Groups = slices.DeleteFunc(d.Groups, func(x string) bool { return x == groupID })
		delete(d.MutedUntil, groupID)
		delete(d.LeaderOnly, groupID)
		if d.CurrentGroup == groupID {
			d.CurrentGroup = ""
			if n := len(d.Groups); n > 0 {
				d.CurrentGroup = d.Groups[n-1]
			}
			evs = append(evs, Event{Type: EvSwitched, DeviceID: id, GroupID: d.CurrentGroup})
		}
	}
	sort.Strings(members)
	delete(s.groups, groupID)
	delete(s.byCode, g.Code)
	s.dirty = true
	return append([]Event{{Type: EvDissolved, GroupID: groupID, Name: g.Name, Members: members}}, evs...), true
}

// IdleTimeout is how long a temporary group may stay silent before it is
// dissolved: the template's timeout, or def.
func IdleTimeout(g GroupView, def time.Duration) time.Duration {
	if t, ok := TemplateByID(g.Template); ok && t.IdleTimeout > 0 {
		return t.IdleTimeout
	}
	return def
}

// DissolveIfIdle dissolves a temporary group only if it has not been active
// since lastActive (as observed by the caller), avoiding check-then-act races.
func (s *Store) DissolveIfIdle(groupID string, lastActive time.Time) bool {
	s.mu.Lock()
	g, ok := s.groups[groupID]
	if !ok || g.Type != GroupTemp || !g.LastActiveAt.Equal(lastActive) {
		s.mu.Unlock()
		return false
	}
	evs, _ := s.dissolve(groupID)
	s.mu.Unlock()
	s.emit(evs...)
	return true
}

// Touch marks the group as active (someone spoke).
func (s *Store) Touch(groupID string) {
	s.mu.Lock()
	if g, ok := s.groups[groupID]; ok {
		g.LastActiveAt = s.now()
		s.dirty = true
	}
	s.mu.Unlock()
}

// MuteGroup silences a group on one device until the given time; a zero
// time unmutes.
func (s *Store) MuteGroup(deviceID, groupID string, until time.Time) error {
	s.mu.Lock()
	g, ok := s.groups[groupID]
	if !ok {
		s.mu.Unlock()
		return ErrNotFound
	}
	if _, ok := g.Members[deviceID]; !ok {
		s.mu.Unlock()
		return ErrNotMember
	}
	d := s.ensureDevice(deviceID)
	if d.MutedUntil == nil {
		d.MutedUntil = map[string]time.Time{}
	}
	if until.IsZero() {
		delete(d.MutedUntil, groupID)
	} else {
		d.MutedUntil[groupID] = until
	}
	s.dirty = true
	s.mu.Unlock()
	s.emit(Event{Type: EvUpdated, DeviceID: deviceID, GroupID: groupID})
	return nil
}

// SetLeaderOnly toggles "只听车队长的" for a device in a group.
func (s *Store) SetLeaderOnly(deviceID, groupID string, on bool) error {
	s.mu.Lock()
	g, ok := s.groups[groupID]
	if !ok {
		s.mu.Unlock()
		return ErrNotFound
	}
	if _, ok := g.Members[deviceID]; !ok {
		s.mu.Unlock()
		return ErrNotMember
	}
	d := s.ensureDevice(deviceID)
	if d.LeaderOnly == nil {
		d.LeaderOnly = map[string]bool{}
	}
	if on {
		d.LeaderOnly[groupID] = true
	} else {
		delete(d.LeaderOnly, groupID)
	}
	s.dirty = true
	s.mu.Unlock()
	s.emit(Event{Type: EvUpdated, DeviceID: deviceID, GroupID: groupID})
	return nil
}

// SetRole promotes or demotes a member. Only a leader may change roles.
func (s *Store) SetRole(actorID, groupID, deviceID string, role Role) error {
	s.mu.Lock()
	g, ok := s.groups[groupID]
	if !ok {
		s.mu.Unlock()
		return ErrNotFound
	}
	if actorID != "" {
		if a, ok := g.Members[actorID]; !ok || a.Role != RoleLeader {
			s.mu.Unlock()
			return ErrForbidden
		}
	}
	m, ok := g.Members[deviceID]
	if !ok {
		s.mu.Unlock()
		return ErrNotMember
	}
	m.Role = role
	s.dirty = true
	s.mu.Unlock()
	s.emit(Event{Type: EvUpdated, GroupID: groupID})
	return nil
}

// ---- discovery ----

// NearbyGroups lists nearby-visible groups within radiusKm, nearest first.
func (s *Store) NearbyGroups(p GeoPoint, radiusKm float64) []GroupView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []GroupView
	for _, g := range s.groups {
		if g.Visibility != VisNearby || g.Location == nil {
			continue
		}
		if dist := DistanceKm(p, *g.Location); dist <= radiusKm {
			v := g.view()
			v.DistanceKm = dist
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DistanceKm < out[j].DistanceKm })
	return out
}

// RouteGroups lists route-visible groups whose route matches the query.
func (s *Store) RouteGroups(query string) []GroupView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []GroupView
	type hit struct {
		v     GroupView
		score float64
	}
	var hits []hit
	for _, g := range s.groups {
		if g.Visibility != VisRoute {
			continue
		}
		score := max(matchScore(query, g.Route), matchScore(query, g.Name))
		if score >= 30 {
			hits = append(hits, hit{g.view(), score})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	for _, h := range hits {
		out = append(out, h.v)
	}
	return out
}

// Resolution is the outcome of resolving a spoken group reference.
type Resolution struct {
	Group      *GroupView  `json:"group,omitempty"`
	Candidates []GroupView `json:"candidates,omitempty"` // ambiguous matches
	ByCode     bool        `json:"by_code,omitempty"`
}

// Resolve turns a free-text reference ("老乡群", "3721", "三七二幺") into a
// group. Room codes match any group; names only match the device's groups.
func (s *Store) Resolve(deviceID, query string) Resolution {
	if code := ParseCode(query); code != "" {
		if g, err := s.GroupByCode(code); err == nil {
			return Resolution{Group: &g, ByCode: true}
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if g, ok := s.groups[strings.TrimSpace(query)]; ok {
		v := g.view()
		return Resolution{Group: &v}
	}
	var mine []*Group
	if d, ok := s.devices[deviceID]; ok {
		for _, id := range d.Groups {
			if g, ok := s.groups[id]; ok {
				mine = append(mine, g)
			}
		}
	}
	ranked := rankGroups(query, mine)
	if len(ranked) == 0 {
		return Resolution{}
	}
	// Unambiguous when the best match clearly beats the runner-up.
	if len(ranked) == 1 || ranked[0].score-ranked[1].score >= 15 || ranked[0].score == 100 {
		v := ranked[0].g.view()
		return Resolution{Group: &v}
	}
	var res Resolution
	for _, r := range ranked {
		if ranked[0].score-r.score < 15 {
			res.Candidates = append(res.Candidates, r.g.view())
		}
	}
	return res
}

// ---- views ----

func (g *Group) view() GroupView {
	v := GroupView{
		ID: g.ID, Code: g.Code, Name: g.Name, Type: g.Type, Visibility: g.Visibility,
		OwnerID: g.OwnerID, Template: g.Template, TalkPolicy: g.TalkPolicy,
		Route: g.Route, CreatedAt: g.CreatedAt, LastActiveAt: g.LastActiveAt,
	}
	if g.Location != nil {
		loc := *g.Location
		v.Location = &loc
	}
	v.Members = make([]Member, 0, len(g.Members))
	for _, m := range g.Members {
		v.Members = append(v.Members, *m)
	}
	sort.Slice(v.Members, func(i, j int) bool { return v.Members[i].JoinedAt.Before(v.Members[j].JoinedAt) })
	return v
}

func (d *Device) view() DeviceView {
	v := DeviceView{
		ID: d.ID, Name: d.Name, CurrentGroup: d.CurrentGroup,
		Groups:     slices.Clone(d.Groups),
		MutedUntil: maps.Clone(d.MutedUntil),
		LeaderOnly: maps.Clone(d.LeaderOnly),
	}
	if d.Location != nil {
		loc := *d.Location
		v.Location = &loc
	}
	return v
}
