// Package config loads server configuration from environment variables.
package config

import (
	"os"
	"strconv"
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
	ASRModel   string
	AlertWords []string

	// Optional outbound MCP endpoint (xiaozhi / Lingxi "MCP 接入点" style).
	MCPEndpoint string
	// Optional shared token required on /mcp and admin APIs.
	AuthToken string
}

func Load() Config {
	return Config{
		Addr:           env("PTT_ADDR", ":8080"),
		DataDir:        env("PTT_DATA_DIR", "./data"),
		BaseURL:        env("PTT_BASE_URL", "http://localhost:8080"),
		TempGroupIdle:  envDuration("PTT_TEMP_GROUP_IDLE", 2*time.Hour),
		NearbyRadiusKm: envFloat("PTT_NEARBY_RADIUS_KM", 3),
		MaxTalk:        envDuration("PTT_MAX_TALK", 60*time.Second),
		AIBaseURL:      env("PTT_AI_BASE_URL", "https://openai.qiniu.com/v1"),
		AIKey:          env("PTT_AI_KEY", ""),
		AIModel:        env("PTT_AI_MODEL", "deepseek-v3"),
		ASRModel:       env("PTT_ASR_MODEL", "asr"),
		AlertWords:     splitList(env("PTT_ALERT_WORDS", "事故,查车,封路,追尾,堵车,危险,救命,着火")),
		MCPEndpoint:    env("PTT_MCP_ENDPOINT", ""),
		AuthToken:      env("PTT_AUTH_TOKEN", ""),
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
