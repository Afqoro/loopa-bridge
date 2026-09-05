package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
)

// Config holds all runtime configuration for the bridge
type Config struct {
	// HTTP server
	Port string
	Host string

	// Loopa WS
	WSEndpoint  string // e.g. wss://www.loopa.im/nanobot-proxy-socket/api/easeclaw/ws/web:new
	AccessToken string // JWT Bearer token

	// Default models
	DefaultModel string
	ImageModel   string
	VideoModel   string
	AudioModel   string
	Timezone     string

	// Bridge auth (optional)
	APIKey string

	// REST API sign params
	ClientName    string // web_app_key, e.g. epm_web
	SignKey       string // e.g. e84yr70o0a5n08f5
	ProductCode   string // e.g. 666
	LocalAPIBase  string // e.g. https://www.loopa.im

	// Auth file fallback
	AuthFile string
}

// LoadConfig reads env vars with sensible defaults
func LoadConfig() *Config {
	cfg := &Config{
		Port:         envOr("LOOPA_PORT", "18768"),
		Host:         envOr("LOOPA_HOST", "0.0.0.0"),
		WSEndpoint:   envOr("LOOPA_WS_ENDPOINT", "wss://www.loopa.im/nanobot-proxy-socket/api/easeclaw/ws/web:new"),
		AccessToken:  envOr("LOOPA_ACCESS_TOKEN", ""),
		DefaultModel: envOr("LOOPA_DEFAULT_MODEL", "minimax/minimax-m3"),
		ImageModel:   envOr("LOOPA_IMAGE_MODEL", "GPT Image 2"),
		VideoModel:   envOr("LOOPA_VIDEO_MODEL", "Seedance 2.0"),
		AudioModel:   envOr("LOOPA_AUDIO_MODEL", "Suno"),
		Timezone:     envOr("LOOPA_TIMEZONE", "Asia/Shanghai"),
		APIKey:       envOr("LOOPA_API_KEY", ""),
		ClientName:   envOr("LOOPA_CLIENT_NAME", "epm_web"),
		SignKey:      envOr("LOOPA_SIGN_KEY", "e84yr70o0a5n08f5"),
		ProductCode:  envOr("LOOPA_PRODUCT_CODE", "666"),
		LocalAPIBase: envOr("LOOPA_LOCAL_API_BASE", "https://www.loopa.im"),
		AuthFile:     envOr("LOOPA_AUTH_FILE", "/tmp/loopa-auth.json"),
	}

	// Fallback: load token from auth file if env var not set
	if cfg.AccessToken == "" {
		if data, err := os.ReadFile(cfg.AuthFile); err == nil {
			var auth struct {
				AccessToken string `json:"access_token"`
			}
			if json.Unmarshal(data, &auth) == nil && auth.AccessToken != "" {
				cfg.AccessToken = auth.AccessToken
				log.Printf("[loopa-bridge] loaded token from %s", cfg.AuthFile)
			}
		}
	}

	return cfg
}

// Listen returns the HTTP server listen address
func (c *Config) Listen() string {
	return fmt.Sprintf("%s:%s", c.Host, c.Port)
}

// WSURL builds the full WebSocket URL with token query param
func (c *Config) WSURL() string {
	u, err := url.Parse(c.WSEndpoint)
	if err != nil {
		// fallback: manual concat
		return c.WSEndpoint + "?token=Bearer%20" + url.QueryEscape(c.AccessToken)
	}
	q := u.Query()
	q.Set("token", "Bearer "+c.AccessToken)
	u.RawQuery = q.Encode()
	return u.String()
}

func envOr(key, def string) string {
	if v := os.Getenv(key); strings.TrimSpace(v) != "" {
		return v
	}
	return def
}
