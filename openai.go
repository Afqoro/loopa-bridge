package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// OpenAI Chat Completions Request
type OAIChatRequest struct {
	Model       string       `json:"model"`
	Messages    []OAIMessage `json:"messages"`
	Stream      bool         `json:"stream"`
	Temperature *float64     `json:"temperature,omitempty"`
	MaxTokens   *int         `json:"max_tokens,omitempty"`
	Tools       []OAITool    `json:"tools,omitempty"`
	ToolChoice  interface{}  `json:"tool_choice,omitempty"`
}

type OAIMessage struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	ToolCalls []OAIToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
	Name      string         `json:"name,omitempty"`
}

type OAITool struct {
	Type     string         `json:"type"`
	Function OAIToolFunction `json:"function"`
}

type OAIToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  interface{} `json:"parameters"`
}

type OAIToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function OAIToolCallFunc  `json:"function"`
}

type OAIToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// OpenAI Chat Completions Response (non-stream)
type OAIChatResponse struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []OAIChoice `json:"choices"`
	Usage   OAIUsage    `json:"usage"`
}

type OAIChoice struct {
	Index        int            `json:"index"`
	Message      OAIMessage     `json:"message"`
	FinishReason string         `json:"finish_reason"`
}

type OAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// OpenAI SSE chunk (stream)
type OAIChunk struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64             `json:"created"`
	Model   string             `json:"model"`
	Choices []OAIChunkChoice  `json:"choices"`
}

type OAIChunkChoice struct {
	Index        int           `json:"index"`
	Delta        OAIDelta      `json:"delta"`
	FinishReason *string       `json:"finish_reason"`
}

type OAIDelta struct {
	Role      string          `json:"role,omitempty"`
	Content   string          `json:"content,omitempty"`
	ToolCalls []OAIToolCall   `json:"tool_calls,omitempty"`
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

	prompt := buildPrompt(req.Messages, req.Tools)
	if prompt == "" {
		writeOAIError(w, http.StatusBadRequest, "invalid_request", "no user message found")
		return
	}

	model := req.Model
	if model == "" {
		model = s.config.DefaultModel
	}

	if req.Stream {
		s.handleStream(w, r, prompt, model, len(req.Tools) > 0)
	} else {
		s.handleNonStream(w, r, prompt, model, len(req.Tools) > 0)
	}
}

func (s *BridgeServer) handleNonStream(w http.ResponseWriter, r *http.Request, prompt, model string, hasTools bool) {
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

	var assistantText strings.Builder
	var toolCalls []OAIToolCall
	var attachments []LoopaAttachment
	var toolCallIdx int
	deadline := time.Now().Add(180 * time.Second)

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
				// Tool call from loopa — convert to OAI tool_calls
				tc := parseLoopaToolCall(msg.Name, msg.Content, &toolCallIdx)
				if tc != nil {
					toolCalls = append(toolCalls, *tc)
				}
			}
			if msg.Status == "completed" {
				attachments = msg.Attachments
				goto done
			}
		}
	}

done:
	finishReason := "stop"
	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
	}

	content := assistantText.String()
	if len(attachments) > 0 {
		content += "\n\n--- Attachments ---\n"
		for _, a := range attachments {
			content += fmt.Sprintf("- %s (%s, %d bytes, id=%s)\n", a.Name, a.ContentType, a.Size, a.FileID)
		}
	}

	resp := OAIChatResponse{
		ID:      fmt.Sprintf("chatcmpl-loopa-%d", time.Now().UnixMilli()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []OAIChoice{
			{
				Index: 0,
				Message: OAIMessage{
					Role:      "assistant",
					Content:   content,
					ToolCalls: toolCalls,
				},
				FinishReason: finishReason,
			},
		},
		Usage: OAIUsage{PromptTokens: len(prompt) / 4, CompletionTokens: len(content) / 4, TotalTokens: (len(prompt) + len(content)) / 4},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *BridgeServer) handleStream(w http.ResponseWriter, r *http.Request, prompt, model string, hasTools bool) {
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

	var toolCallIdx int
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
				if msg.Name == "" {
					sendSSEChunk(w, flusher, chatID, created, model, &OAIDelta{Content: msg.Content}, nil)
				} else {
					// Tool call — emit as proper OAI tool_calls delta
					tc := parseLoopaToolCall(msg.Name, msg.Content, &toolCallIdx)
					if tc != nil {
						sendSSEChunk(w, flusher, chatID, created, model, &OAIDelta{ToolCalls: []OAIToolCall{*tc}}, nil)
					}
				}
			} else if msg.Role == "tool" {
				// Tool result — stream as content (for visibility) but don't emit as separate tool_calls
				sendSSEChunk(w, flusher, chatID, created, model, &OAIDelta{Content: fmt.Sprintf("[tool output] %s\n", msg.Content)}, nil)
			}
			if msg.Status == "completed" {
				for _, a := range msg.Attachments {
					sendSSEChunk(w, flusher, chatID, created, model, &OAIDelta{Content: fmt.Sprintf("\n[attachment] %s (%d bytes, id=%s)\n", a.Name, a.Size, a.FileID)}, nil)
				}
				reason := "stop"
				if toolCallIdx > 0 {
					reason = "tool_calls"
				}
				sendSSEChunk(w, flusher, chatID, created, model, &OAIDelta{}, &reason)
				fmt.Fprintf(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
		}
	}

	reason := "length"
	sendSSEChunk(w, flusher, chatID, created, model, &OAIDelta{}, &reason)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// parseLoopaToolCall converts a loopa WS tool-call message into an OpenAI tool_calls entry.
// Loopa format: role=assistant, name="exec|read_file|write_file", content="exec(\"cmd\")" or "read_file(\"path\")"
func parseLoopaToolCall(toolName, content string, idx *int) *OAIToolCall {
	*idx++
	id := fmt.Sprintf("call_loopa_%d", *idx)

	// Extract arguments as JSON string
	args := extractToolArgs(toolName, content)

	return &OAIToolCall{
		ID:   id,
		Type: "function",
		Function: OAIToolCallFunc{
			Name:      toolName,
			Arguments: args,
		},
	}
}

// extractToolArgs parses loopa tool content into JSON arguments string
// e.g. exec("ls -la") -> {"command":"ls -la"}
//      read_file("/tmp/foo") -> {"path":"/tmp/foo"}
//      write_file("/tmp/foo", "content...") -> {"path":"/tmp/foo","content":"content..."}
func extractToolArgs(toolName, content string) string {
	switch toolName {
	case "exec":
		cmd := extractQuotedString(content)
		args, _ := json.Marshal(map[string]string{"command": cmd})
		return string(args)
	case "read_file":
		path := extractQuotedString(content)
		args, _ := json.Marshal(map[string]string{"path": path})
		return string(args)
	case "write_file":
		// Try to extract path + content (two quoted strings)
		parts := extractTwoQuotedStrings(content)
		if len(parts) >= 2 {
			args, _ := json.Marshal(map[string]string{"path": parts[0], "content": parts[1]})
			return string(args)
		}
		// Fallback: just path
		path := extractQuotedString(content)
		args, _ := json.Marshal(map[string]string{"path": path})
		return string(args)
	default:
		// Unknown tool — pass content as-is in "input" field
		args, _ := json.Marshal(map[string]string{"input": content})
		return string(args)
	}
}

// extractQuotedString extracts the first double-quoted string from content
var quotedStringRe = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)

func extractQuotedString(content string) string {
	m := quotedStringRe.FindStringSubmatch(content)
	if len(m) >= 2 {
		return m[1]
	}
	return content
}

// extractTwoQuotedStrings extracts first two double-quoted strings from content
func extractTwoQuotedStrings(content string) []string {
	matches := quotedStringRe.FindAllStringSubmatch(content, 2)
	var result []string
	for _, m := range matches {
		if len(m) >= 2 {
			result = append(result, m[1])
		}
	}
	return result
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

// buildPrompt flattens OpenAI messages + tools into a single prompt string for loopa WS
func buildPrompt(messages []OAIMessage, tools []OAITool) string {
	var parts []string
	var userMsg string

	// If tools are provided, inject as system instruction so loopa model knows available tools
	if len(tools) > 0 {
		var toolDefs []string
		for _, t := range tools {
			td := fmt.Sprintf("- %s: %s", t.Function.Name, t.Function.Description)
			if t.Function.Parameters != nil {
				if pj, err := json.Marshal(t.Function.Parameters); err == nil {
					td += fmt.Sprintf(" (params: %s)", string(pj))
				}
			}
			toolDefs = append(toolDefs, td)
		}
		parts = append(parts, fmt.Sprintf("[Available Tools]\n%s", strings.Join(toolDefs, "\n")))
	}

	for _, m := range messages {
		switch m.Role {
		case "system":
			parts = append(parts, fmt.Sprintf("[System]\n%s", m.Content))
		case "user":
			userMsg = m.Content
		case "assistant":
			if m.Content != "" {
				parts = append(parts, fmt.Sprintf("[Previous Assistant]\n%s", m.Content))
			}
			if len(m.ToolCalls) > 0 {
				for _, tc := range m.ToolCalls {
					parts = append(parts, fmt.Sprintf("[Previous Tool Call]\n%s(%s)", tc.Function.Name, tc.Function.Arguments))
				}
			}
		case "tool":
			parts = append(parts, fmt.Sprintf("[Tool Output: %s]\n%s", m.Name, m.Content))
		}
	}
	if len(parts) > 0 && userMsg != "" {
		return strings.Join(parts, "\n\n") + "\n\n---\n\n" + userMsg
	}
	return userMsg
}
