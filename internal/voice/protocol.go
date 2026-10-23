// Package voice implements the real-time push-to-talk relay: device
// connections, floor control ("抢麦") and Opus frame fan-out.
//
// Wire protocol (see docs/protocol.md): JSON text frames for signalling,
// binary frames carrying one raw Opus packet each (16 kHz mono, 60 ms).
package voice

import "time"

// Client → server message types.
const (
	MsgHello    = "hello"
	MsgPTTStart = "ptt_start"
	MsgPTTStop  = "ptt_stop"
	MsgSwitch   = "switch"
	MsgLocation = "location"
	MsgPing     = "ping"
	// MsgTranscript lets a capable client (e.g. the browser simulator with
	// Web Speech API) supply the text of its own utterance.
	MsgTranscript = "transcript"
	// MsgPair: the "碰一碰" button; two devices pressing within a few
	// seconds (and close by, when GPS is known) get a fresh temp group.
	MsgPair = "pair"
)

// Server → client message types.
const (
	MsgWelcome      = "welcome"
	MsgChannel      = "channel"
	MsgFloor        = "floor"
	MsgFloorRevoked = "floor_revoked"
	MsgTalkStart    = "talk_start"
	MsgTalkStop     = "talk_stop"
	MsgPresence     = "presence"
	MsgNotice       = "notice"
	MsgPong         = "pong"
	MsgError        = "error"
)

// Inbound is the union of all client → server JSON messages.
type Inbound struct {
	Type    string  `json:"type"`
	Name    string  `json:"name,omitempty"`
	GroupID string  `json:"group_id,omitempty"`
	Query   string  `json:"query,omitempty"`
	Delta   int     `json:"delta,omitempty"`
	Lat     float64 `json:"lat,omitempty"`
	Lng     float64 `json:"lng,omitempty"`
	// transcript
	UtteranceID string `json:"utterance_id,omitempty"`
	Text        string `json:"text,omitempty"`
}

// AudioParams describes the codec every device must speak.
type AudioParams struct {
	Format     string `json:"format"`
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
	FrameMs    int    `json:"frame_duration"`
}

var DefaultAudio = AudioParams{Format: "opus", SampleRate: 16000, Channels: 1, FrameMs: 60}

// Priority of a talk stream; higher preempts lower on a listener's speaker.
type Priority int

const (
	PriorityReplay    Priority = 0 // archived playback; any live talk interrupts it
	PriorityNormal    Priority = 1
	PriorityLeader    Priority = 2
	PriorityEmergency Priority = 3
)

// Utterance is one continuous PTT transmission.
type Utterance struct {
	ID          string      `json:"id"`
	GroupID     string      `json:"group_id"`
	GroupName   string      `json:"group_name"`
	SpeakerID   string      `json:"speaker_id"`
	SpeakerName string      `json:"speaker_name"`
	Leader      bool        `json:"leader"`
	Priority    Priority    `json:"priority"`
	StartedAt   time.Time   `json:"started_at"`
	EndedAt     time.Time   `json:"ended_at,omitzero"`
	Frames      int         `json:"frames"`
	Audio       AudioParams `json:"audio"`

	lastFrame time.Time
}

func (u *Utterance) Duration() time.Duration {
	end := u.EndedAt
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(u.StartedAt)
}

// Recorder receives every utterance for archiving / transcription. Calls are
// made synchronously from the relay hot path and must not block.
type Recorder interface {
	Start(u Utterance)
	Frame(utteranceID string, pkt []byte)
	End(u Utterance)
}

type talkMsg struct {
	Type        string   `json:"type"`
	UtteranceID string   `json:"utterance_id"`
	GroupID     string   `json:"group_id"`
	GroupName   string   `json:"group_name"`
	SpeakerID   string   `json:"speaker_id"`
	SpeakerName string   `json:"speaker_name"`
	Leader      bool     `json:"leader,omitempty"`
	Priority    Priority `json:"priority"`
	Reason      string   `json:"reason,omitempty"`
	DurationMs  int64    `json:"duration_ms,omitempty"`
	// Replay marks archived audio played back on request.
	Replay bool `json:"replay,omitempty"`
}

type floorMsg struct {
	Type        string `json:"type"`
	Granted     bool   `json:"granted"`
	GroupID     string `json:"group_id,omitempty"`
	UtteranceID string `json:"utterance_id,omitempty"`
	Reason      string `json:"reason,omitempty"`
	HolderID    string `json:"holder_id,omitempty"`
	HolderName  string `json:"holder_name,omitempty"`
	MaxMs       int64  `json:"max_ms,omitempty"`
}

type presenceMsg struct {
	Type        string   `json:"type"`
	GroupID     string   `json:"group_id"`
	OnlineCount int      `json:"online_count"`
	Online      []string `json:"online"`
	MemberCount int      `json:"member_count"`
}

type noticeMsg struct {
	Type    string `json:"type"`
	Level   string `json:"level"` // info | alert
	Text    string `json:"text"`  // short sentence suitable for TTS
	GroupID string `json:"group_id,omitempty"`
}
