package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// LoopaWSMessage is the message sent by client to loopa.im WS
type LoopaWSMessage struct {
	Type        string        `json:"type"`
	Content     string        `json:"content"`
	Attachments []interface{} `json:"attachments"`
	SessionKey  string        `json:"session_key"`
	Model       string        `json:"model"`
	ImageModel  string        `json:"image_model"`
	VideoModel  string        `json:"video_model"`
	AudioModel  string        `json:"audio_model"`
	MCP         []interface{} `json:"mcp"`
	Timezone    string        `json:"timezone"`
	BrowserID   string        `json:"browserId"`
}

// LoopaWSResponse is a message received from loopa.im WS
type LoopaWSResponse struct {
	Type            string                 `json:"type"`
	Status          string                 `json:"status"`
	Role            string                 `json:"role"`
	Name            string                 `json:"name"` // tool name: exec, read_file, write_file
	Content         string                 `json:"content"`
	SessionKey      string                 `json:"session_key"`
	ConversationID  string                 `json:"conversation_id"`
	Channel         string                 `json:"channel"`
	Attachments      []LoopaAttachment     `json:"attachments"`
	Raw             map[string]interface{} `json:"-"`
}

type LoopaAttachment struct {
	FileID      string `json:"file_id"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	CreatedAt   string `json:"created_at"`
	SessionID   string `json:"session_id"`
}

var dialer = websocket.Dialer{
	HandshakeTimeout: 15 * time.Second,
}

// generateSessionKey creates a new loopa.im session key: web:session-<ms>-<rand6>
func generateSessionKey() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	b := make([]byte, 6)
	for i := range b {
		b[i] = chars[r.Intn(len(chars))]
	}
	return fmt.Sprintf("web:session-%d-%s", time.Now().UnixMilli(), string(b))
}

// LoopaClient manages a single WS connection to loopa.im
// One connection per chat request — loopa WS is stateful per session.
type LoopaClient struct {
	conn       *websocket.Conn
	config     *Config
	sessionKey string
	done       chan struct{}
	mu         sync.Mutex
}

// NewLoopaClient connects to loopa.im WS and prepares a new session
func NewLoopaClient(cfg *Config) (*LoopaClient, error) {
	header := http.Header{}
	header.Set("Origin", "https://www.loopa.im")
	header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36")

	conn, resp, err := dialer.Dial(cfg.WSURL(), header)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("ws dial: %w (status %d)", err, resp.StatusCode)
		}
		return nil, fmt.Errorf("ws dial: %w", err)
	}
	if resp != nil {
		resp.Body.Close()
	}

	sessionKey := generateSessionKey()
	return &LoopaClient{
		conn:       conn,
		config:     cfg,
		sessionKey: sessionKey,
		done:       make(chan struct{}),
	}, nil
}

// SendMessage sends a chat message to loopa.im via WS
func (c *LoopaClient) SendMessage(content, model string) error {
	if model == "" {
		model = c.config.DefaultModel
	}
	msg := LoopaWSMessage{
		Type:        "message",
		Content:     content,
		Attachments: []interface{}{},
		SessionKey:  c.sessionKey,
		Model:       model,
		ImageModel:  c.config.ImageModel,
		VideoModel:  c.config.VideoModel,
		AudioModel:  c.config.AudioModel,
		MCP:         []interface{}{},
		Timezone:    c.config.Timezone,
		BrowserID:   "",
	}
	return c.conn.WriteJSON(msg)
}

// SendPing sends a keepalive ping
func (c *LoopaClient) SendPing() error {
	return c.conn.WriteJSON(map[string]string{"type": "ping"})
}

// ReadMessage reads one JSON message from WS with a deadline
func (c *LoopaClient) ReadMessage(timeout time.Duration) (*LoopaWSResponse, error) {
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	_, data, err := c.conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	var resp LoopaWSResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal: %w (raw: %s)", err, string(data[:min(len(data), 200)]))
	}
	return &resp, nil
}

// Close closes the WS connection
func (c *LoopaClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		// already closed
	default:
		close(c.done)
	}
	_ = c.conn.Close()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
