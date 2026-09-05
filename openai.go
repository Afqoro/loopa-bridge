package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// OpenAI Chat Completions Request
type OAIChatRequest struct {
	Model       string      `json:"model"`
	Messages    []OAIMessage `json:"messages"`
	Stream      bool        `json:"stream"`
	Temperature *float64    `json:"temperature,omitempty"`
	MaxTokens   *int        `json:"max_tokens,omitempty"`
}

type OAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// OpenAI Chat Completions Response (non-stream)
type OAIChatResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OAIChoice    `json:"choices"`
	Usage   OAIUsage       `json:"usage"`
}

type OAIChoice struct {
	Index        int       `json:"index"`
	Message      OAIMessage `json:"message"`
	FinishReason string    `json:"finish_reason"`
}

type OAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// OpenAI SSE chunk (stream)
type OAIChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []OAIChunkChoice `json:"choices"`
}

type OAIChunkChoice struct {
	Index        int          `json:"index"`
	Delta        OAIDelta     `json:"delta"`
	FinishReason *string      `json:"finish_reason"`
}

type OAIDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// HandleChatCompletions is the main OpenAI-compatible endpoint
type BridgeServer struct {
	config *Config
}

func NewBridgeServer(cfg *Config) *BridgeServer {
	return &BridgeServer{config: cfg}
}

func (s *BridgeServer) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Auth check (optional)
	if s.config.APIKey != "" {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != s.config.APIKey {
			writeOAIError(w, http.StatusUnauthorized, "invalid_api_key", "Invalid API key")
			return
		}
	}

	var req OAIChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// Flatten messages into a single prompt (loopa WS only takes a single content string)
	// Use last user message as the prompt, prepend system context if present
	prompt := buildPrompt(req.Messages)
	if prompt == "" {
		writeOAIError(w, http.StatusBadRequest, "invalid_request", "no user message found")
		return
	}

	model := req.Model
	if model == "" {
		model = s.config.DefaultModel
	}

	if req.Stream {
		s.handleStream(w, r, prompt, model)
	} else {
		s.handleNonStream(w, r, prompt, model)
	}
}

func (s *BridgeServer) handleNonStream(w http.ResponseWriter, r *http.Request, prompt, model string) {
	client, err := NewLoopaClient(s.config)
	if err != nil {
		writeOAIError(w, http.StatusBadGateway, "upstream_error", fmt.Sprintf("ws connect: %v", err))
		return
	}
	defer client.Close()

	if err := client.SendMessage(prompt, model); err != nil {
		writeOAIError(w, http.StatusBadGateway, "upstream_error", fmt.Sprintf("send: %v", err))
		return
	}

	// Collect all messages until status=completed
	var assistantText strings.Builder
	var toolOutputs []string
	var attachments []LoopaAttachment
	deadline := time.Now().Add(120 * time.Second)

	for time.Now().Before(deadline) {
		msg, err := client.ReadMessage(120 * time.Second)
		if err != nil {
			writeOAIError(w, http.StatusBadGateway, "upstream_error", fmt.Sprintf("read: %v", err))
			return
		}

		switch msg.Type {
		case "ping":
			_ = client.SendPing()
			continue
		case "pong":
			continue
		case "status":
			if msg.Status == "completed" || msg.Status == "error" {
				goto done
			}
		case "message":
			if msg.Role == "assistant" && msg.Name == "" {
				if assistantText.Len() > 0 {
					assistantText.WriteString("\n\n")
				}
				assistantText.WriteString(msg.Content)
			} else if msg.Role == "assistant" && msg.Name != "" {
				// tool call — include as text
				toolOutputs = append(toolOutputs, fmt.Sprintf("[%s] %s", msg.Name, msg.Content))
			} else if msg.Role == "tool" {
				toolOutputs = append(toolOutputs, fmt.Sprintf("[tool:%s] %s", msg.Name, msg.Content))
			}
			if msg.Status == "completed" {
				attachments = msg.Attachments
				goto done
			}
		}
	}

done:
	fullText := assistantText.String()
	if len(toolOutputs) > 0 {
		fullText += "\n\n--- Tool Output ---\n" + strings.Join(toolOutputs, "\n\n")
	}
	if len(attachments) > 0 {
		fullText += "\n\n--- Attachments ---\n"
		for _, a := range attachments {
			fullText += fmt.Sprintf("- %s (%s, %d bytes, id=%s)\n", a.Name, a.ContentType, a.Size, a.FileID)
		}
	}

	resp := OAIChatResponse{
		ID:      fmt.Sprintf("chatcmpl-loopa-%d", time.Now().UnixMilli()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []OAIChoice{
			{
				Index:        0,
				Message:      OAIMessage{Role: "assistant", Content: fullText},
				FinishReason: "stop",
			},
		},
		Usage: OAIUsage{PromptTokens: len(prompt) / 4, CompletionTokens: len(fullText) / 4, TotalTokens: (len(prompt) + len(fullText)) / 4},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *BridgeServer) handleStream(w http.ResponseWriter, r *http.Request, prompt, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	client, err := NewLoopaClient(s.config)
	if err != nil {
		sendSSEError(w, flusher, fmt.Sprintf("ws connect: %v", err))
		return
	}
	defer client.Close()

	if err := client.SendMessage(prompt, model); err != nil {
		sendSSEError(w, flusher, fmt.Sprintf("send: %v", err))
		return
	}

	chatID := fmt.Sprintf("chatcmpl-loopa-%d", time.Now().UnixMilli())
	created := time.Now().Unix()

	// Send initial role chunk
	sendSSEChunk(w, flusher, chatID, created, model, &OAIDelta{Role: "assistant"}, nil)

	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		msg, err := client.ReadMessage(120 * time.Second)
		if err != nil {
			sendSSEError(w, flusher, fmt.Sprintf("read: %v", err))
			return
		}

		switch msg.Type {
		case "ping":
			_ = client.SendPing()
			continue
		case "pong":
			continue
		case "status":
			if msg.Status == "completed" || msg.Status == "error" {
				reason := "stop"
				sendSSEChunk(w, flusher, chatID, created, model, &OAIDelta{}, &reason)
				fmt.Fprintf(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
		case "message":
			if msg.Role == "assistant" && msg.Content != "" {
				// Stream assistant content (skip tool call internals for clean text)
				if msg.Name == "" {
					sendSSEChunk(w, flusher, chatID, created, model, &OAIDelta{Content: msg.Content + "\n"}, nil)
				} else {
					// Tool call — send as content with marker
					sendSSEChunk(w, flusher, chatID, created, model, &OAIDelta{Content: fmt.Sprintf("\n[tool:%s] %s\n", msg.Name, msg.Content)}, nil)
				}
			} else if msg.Role == "tool" {
				sendSSEChunk(w, flusher, chatID, created, model, &OAIDelta{Content: fmt.Sprintf("[output] %s\n", msg.Content)}, nil)
			}
			if msg.Status == "completed" {
				// Send attachments info if any
				for _, a := range msg.Attachments {
					sendSSEChunk(w, flusher, chatID, created, model, &OAIDelta{Content: fmt.Sprintf("\n[attachment] %s (%d bytes, id=%s)\n", a.Name, a.Size, a.FileID)}, nil)
				}
				reason := "stop"
				sendSSEChunk(w, flusher, chatID, created, model, &OAIDelta{}, &reason)
				fmt.Fprintf(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
		}
	}

	// Timeout
	reason := "length"
	sendSSEChunk(w, flusher, chatID, created, model, &OAIDelta{}, &reason)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func sendSSEChunk(w http.ResponseWriter, flusher http.Flusher, id string, created int64, model string, delta *OAIDelta, finishReason *string) {
	chunk := OAIChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []OAIChunkChoice{
			{Index: 0, Delta: *delta, FinishReason: finishReason},
		},
	}
	data, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

func sendSSEError(w http.ResponseWriter, flusher http.Flusher, msg string) {
	errObj := map[string]interface{}{
		"error": map[string]interface{}{
			"message": msg,
			"type":    "upstream_error",
			"code":    "upstream_error",
		},
	}
	data, _ := json.Marshal(errObj)
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

func writeOAIError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": message,
			"type":    errType,
			"code":    errType,
		},
	})
}

// buildPrompt flattens OpenAI messages into a single prompt string
func buildPrompt(messages []OAIMessage) string {
	var parts []string
	var userMsg string
	for _, m := range messages {
		switch m.Role {
		case "system":
			parts = append(parts, fmt.Sprintf("[System]\n%s", m.Content))
		case "user":
			userMsg = m.Content
		case "assistant":
			parts = append(parts, fmt.Sprintf("[Previous Assistant]\n%s", m.Content))
		case "tool":
			parts = append(parts, fmt.Sprintf("[Tool Output]\n%s", m.Content))
		}
	}
	// Prepend context, then user message
	if len(parts) > 0 && userMsg != "" {
		return strings.Join(parts, "\n\n") + "\n\n---\n\n" + userMsg
	}
	return userMsg
}
