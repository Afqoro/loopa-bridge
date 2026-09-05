package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	cfg := LoadConfig()

	if cfg.AccessToken == "" {
		log.Fatal("LOOPA_ACCESS_TOKEN is required (or /tmp/loopa-auth.json must exist)")
	}

	log.Printf("[loopa-bridge] starting on %s", cfg.Listen())
	log.Printf("[loopa-bridge] default model: %s", cfg.DefaultModel)
	log.Printf("[loopa-bridge] WS: %s", cfg.WSEndpoint)
	if cfg.APIKey != "" {
		log.Printf("[loopa-bridge] auth: enabled (LOOPA_API_KEY)")
	} else {
		log.Printf("[loopa-bridge] auth: disabled (set LOOPA_API_KEY to enable)")
	}

	bridge := NewBridgeServer(cfg)
	mux := http.NewServeMux()

	// OpenAI-compatible endpoints
	mux.HandleFunc("/v1/chat/completions", bridge.HandleChatCompletions)

	// Health & info
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":  "ok",
			"service": "loopa-bridge",
			"version": "0.1.0",
			"model":   cfg.DefaultModel,
			"time":    time.Now().UTC().Format(time.RFC3339),
		})
	})

	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"object": "list",
			"data": []map[string]interface{}{
				{"id": "minimax/minimax-m3", "object": "model", "owned_by": "loopa"},
				{"id": "deepseek/deepseek-v4-flash-0731", "object": "model", "owned_by": "loopa"},
				{"id": "moonshotai/kimi-k2.5", "object": "model", "owned_by": "loopa"},
				{"id": "qwen/qwen3.6-flash", "object": "model", "owned_by": "loopa"},
			},
		})
	})

	// Credits endpoint - check remaining loopa.im credits via signed REST API
	mux.HandleFunc("/v1/credits", func(w http.ResponseWriter, r *http.Request) {
		perm, err := CheckCredit(cfg)
		if err != nil {
			writeOAIError(w, http.StatusBadGateway, "credit_check_failed", err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"free_credits":    perm.Data.FreeCredits,
			"credits":         perm.Data.Credits,
			"images":          perm.Data.Images,
			"musics":          perm.Data.Musics,
			"vip":             perm.Data.VIP,
			"raw":             perm,
		})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"service":   "loopa-bridge",
			"version":   "0.1.0",
			"endpoints": []string{"/v1/chat/completions", "/v1/models", "/healthz"},
			"repo":      "https://github.com/Afqoro/loopa-bridge",
		})
	})

	server := &http.Server{
		Addr:              cfg.Listen(),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0, // streaming
		WriteTimeout:      0, // streaming
		IdleTimeout:       120 * time.Second,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("[loopa-bridge] shutting down...")
		_ = server.Close()
		os.Exit(0)
	}()

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[loopa-bridge] server error: %v", err)
	}

	fmt.Println("[loopa-bridge] stopped")
}
