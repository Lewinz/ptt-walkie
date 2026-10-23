// Package config loads server configuration from environment variables.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr    string // HTTP listen address
	DataDir string // state snapshot + recordings
	BaseURL string // public URL used in share links / QR codes

	// Temporary groups are dissolved after this long without any speech.
	TempGroupIdle time.Duration
	// Default radius for "nearby" group discovery.
	NearbyRadiusKm float64
	// Max length of a single PTT utterance; the floor is force-released after this.
	MaxTalk time.Duration

	// OpenAI-compatible endpoint for transcription / summarisation (optional).
	AIBaseURL  string
	AIKey      string
	AIModel    string
	ASRMode    string // "qiniu" (POST /voice/asr with audio URL) or "openai" (/audio/transcriptions upload)
	ASRModel   string
	AlertWords []string

	// Optional outbound MCP endpoints (xiaozhi / Lingxi "MCP 接入点" style):
	// "wss://…?token=…|device_id; wss://…|device_id2".
	MCPEndpoint string
	// Optional shared token required on /mcp.
	AuthToken string
	// Optional TLS. Browsers only grant microphone access on HTTPS (or
	// localhost), so set these when serving the simulator on a LAN / domain.
	TLSCert string
	TLSKey  string

	// Optional Qiniu Kodo mirror for recordings (S3-compatible API).
	KodoEndpoint string // https://s3.cn-east-1.qiniucs.com
	KodoRegion   string // derived from the endpoint when empty
	KodoBucket   string
	KodoAK       string
	KodoSK       string
	KodoPrefix   string

	// Demo seeds showcase groups and runs a built-in echo (radio-check) bot.
	Demo bool
}

// KodoEnabled reports whether recordings should be mirrored to Kodo.
func (c Config) KodoEnabled() bool {
	return c.KodoBucket != "" && c.KodoAK != "" && c.KodoSK != ""
}

// KodoRegionOrDefault derives the region from s3.{region}.qiniucs.com.
func (c Config) KodoRegionOrDefault() string {
	if c.KodoRegion != "" {
		return c.KodoRegion
	}
	host := c.KodoEndpoint
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if parts := strings.Split(host, "."); len(parts) > 2 && parts[0] == "s3" {
		return parts[1]
	}
	return "cn-east-1"
}

func Load() Config {
	return Config{
		Addr:           env("PTT_ADDR", ":8080"),
		DataDir:        env("PTT_DATA_DIR", "./data"),
		BaseURL:        env("PTT_BASE_URL", "http://localhost:8080"),
		TempGroupIdle:  envDuration("PTT_TEMP_GROUP_IDLE", 2*time.Hour),
		NearbyRadiusKm: envFloat("PTT_NEARBY_RADIUS_KM", 3),
		MaxTalk:        envDuration("PTT_MAX_TALK", 60*time.Second),
		AIBaseURL:      env("PTT_AI_BASE_URL", "https://api.qnaigc.com/v1"),
		AIKey:          env("PTT_AI_KEY", ""),
		AIModel:        env("PTT_AI_MODEL", "deepseek-v3"),
		ASRMode:        env("PTT_ASR_MODE", "qiniu"),
		ASRModel:       env("PTT_ASR_MODEL", "asr"),
		AlertWords:     splitList(env("PTT_ALERT_WORDS", "事故,查车,封路,追尾,堵车,危险,救命,着火")),
		MCPEndpoint:    env("PTT_MCP_ENDPOINT", ""),
		AuthToken:      env("PTT_AUTH_TOKEN", ""),
		TLSCert:        env("PTT_TLS_CERT", ""),
		TLSKey:         env("PTT_TLS_KEY", ""),
		KodoEndpoint:   env("PTT_KODO_ENDPOINT", "https://s3.cn-east-1.qiniucs.com"),
		KodoRegion:     env("PTT_KODO_REGION", ""),
		KodoBucket:     env("PTT_KODO_BUCKET", ""),
		KodoAK:         env("PTT_KODO_AK", ""),
		KodoSK:         env("PTT_KODO_SK", ""),
		KodoPrefix:     env("PTT_KODO_PREFIX", "ptt-walkie/"),
		Demo:           env("PTT_DEMO", "") == "1" || env("PTT_DEMO", "") == "true",
	}
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func envDuration(k string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(k)); err == nil {
		return d
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if f, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return f
	}
	return def
}

func splitList(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	return out
}
