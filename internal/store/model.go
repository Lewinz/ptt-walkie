// Package store holds the group / device domain model and its persistence.
package store

import (
	"errors"
	"time"
)

type GroupType string

const (
	GroupTemp      GroupType = "temp"      // nearby / route / ad-hoc; dissolved after idle
	GroupFixed     GroupType = "fixed"     // fleet, hometown, family …
	GroupEmergency GroupType = "emergency" // highest priority, never dissolved
)

type Visibility string

const (
	VisCode   Visibility = "code"   // join by room code / QR only
	VisNearby Visibility = "nearby" // discoverable by GPS radius
	VisRoute  Visibility = "route"  // discoverable by route / destination
)

type Role string

const (
	RoleLeader Role = "leader"
	RoleMember Role = "member"
)

type GeoPoint struct {
	Lat float64 `json:"lat"`
	Lng float64 `json:"lng"`
}

type Member struct {
	DeviceID string    `json:"device_id"`
	Role     Role      `json:"role"`
	JoinedAt time.Time `json:"joined_at"`
}

type Group struct {
	ID           string             `json:"id"`
	Code         string             `json:"code"`
	Name         string             `json:"name"`
	Type         GroupType          `json:"type"`
	Visibility   Visibility         `json:"visibility"`
	OwnerID      string             `json:"owner_id"`
	Route        string             `json:"route,omitempty"`
	Location     *GeoPoint          `json:"location,omitempty"`
	Members      map[string]*Member `json:"members"`
	CreatedAt    time.Time          `json:"created_at"`
	LastActiveAt time.Time          `json:"last_active_at"`
}

type Device struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	CurrentGroup string    `json:"current_group,omitempty"`
	Groups       []string  `json:"groups"` // join order, used for next / previous channel
	Location     *GeoPoint `json:"location,omitempty"`
	LocationAt   time.Time `json:"location_at,omitzero"`
	// Per-group mute deadline. Zero time means not muted.
	MutedUntil map[string]time.Time `json:"muted_until,omitempty"`
	// Per-group "only listen to leaders" switch.
	LeaderOnly map[string]bool `json:"leader_only,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// GroupView is an immutable snapshot of a group returned to callers.
type GroupView struct {
	ID           string     `json:"id"`
	Code         string     `json:"code"`
	Name         string     `json:"name"`
	Type         GroupType  `json:"type"`
	Visibility   Visibility `json:"visibility"`
	OwnerID      string     `json:"owner_id"`
	Route        string     `json:"route,omitempty"`
	Location     *GeoPoint  `json:"location,omitempty"`
	Members      []Member   `json:"members"`
	CreatedAt    time.Time  `json:"created_at"`
	LastActiveAt time.Time  `json:"last_active_at"`
	DistanceKm   float64    `json:"distance_km,omitempty"`
}

func (g GroupView) IsLeader(deviceID string) bool {
	for _, m := range g.Members {
		if m.DeviceID == deviceID {
			return m.Role == RoleLeader
		}
	}
	return false
}

func (g GroupView) HasMember(deviceID string) bool {
	for _, m := range g.Members {
		if m.DeviceID == deviceID {
			return true
		}
	}
	return false
}

// DeviceView is an immutable snapshot of a device.
type DeviceView struct {
	ID           string               `json:"id"`
	Name         string               `json:"name"`
	CurrentGroup string               `json:"current_group,omitempty"`
	Groups       []string             `json:"groups"`
	Location     *GeoPoint            `json:"location,omitempty"`
	MutedUntil   map[string]time.Time `json:"muted_until,omitempty"`
	LeaderOnly   map[string]bool      `json:"leader_only,omitempty"`
}

func (d DeviceView) IsMuted(groupID string, now time.Time) bool {
	return now.Before(d.MutedUntil[groupID])
}

var (
	ErrNotFound     = errors.New("not found")
	ErrNotMember    = errors.New("device is not a member of the group")
	ErrInvalidInput = errors.New("invalid input")
	ErrForbidden    = errors.New("forbidden")
)

// EventType enumerates changes the voice plane needs to react to.
type EventType string

const (
	EvSwitched  EventType = "switched"  // device's current group changed
	EvJoined    EventType = "joined"    // device joined a group
	EvLeft      EventType = "left"      // device left a group
	EvDissolved EventType = "dissolved" // group removed; Members lists former members
	EvUpdated   EventType = "updated"   // group metadata / mute settings changed
)

type Event struct {
	Type     EventType `json:"type"`
	DeviceID string    `json:"device_id,omitempty"`
	GroupID  string    `json:"group_id,omitempty"`
	Name     string    `json:"name,omitempty"`    // group name, set on dissolve
	Members  []string  `json:"members,omitempty"` // former members, set on dissolve
}
