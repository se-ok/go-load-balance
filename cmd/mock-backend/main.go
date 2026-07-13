package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"
)

// Mock backend server for testing the load balancer
// Supports various behaviors: healthy, slow, failing, flaky

type Config struct {
	Port           int
	Mode           string
	Delay          time.Duration
	FailureRate    float64
	ResponseSize   int
	HealthEndpoint string
}

func main() {
	config := Config{}

	flag.IntVar(&config.Port, "port", 8000, "Port to listen on")
	flag.StringVar(&config.Mode, "mode", "healthy", "Mode: healthy, slow, failing, flaky, timeout")
	flag.DurationVar(&config.Delay, "delay", 0, "Response delay duration (e.g., 100ms, 5s, 10m)")
	flag.Float64Var(&config.FailureRate, "failure-rate", 0.5, "Failure rate for flaky mode (0.0-1.0)")
	flag.IntVar(&config.ResponseSize, "response-size", 1024, "Response body size in bytes")
	flag.StringVar(&config.HealthEndpoint, "health-endpoint", "/v1/models", "Health check endpoint")

	flag.Parse()

	// Validate config
	if config.Port < 1 || config.Port > 65535 {
		fmt.Fprintf(os.Stderr, "Error: invalid port %d\n", config.Port)
		os.Exit(1)
	}

	if config.FailureRate < 0 || config.FailureRate > 1 {
		fmt.Fprintf(os.Stderr, "Error: failure-rate must be between 0.0 and 1.0\n")
		os.Exit(1)
	}

	log.Printf("Starting mock backend server")
	log.Printf("  Port: %d", config.Port)
	log.Printf("  Mode: %s", config.Mode)
	log.Printf("  Delay: %v", config.Delay)
	log.Printf("  Response size: %d bytes", config.ResponseSize)
	if config.Mode == "flaky" {
		log.Printf("  Failure rate: %.1f%%", config.FailureRate*100)
	}

	// Create handler
	handler := &BackendHandler{config: config}

	// Register routes
	http.HandleFunc(config.HealthEndpoint, handler.handleHealth)
	http.HandleFunc("/v1/completions", handler.handleCompletions)
	http.HandleFunc("/v1/chat/completions", handler.handleCompletions)
	http.HandleFunc("/prefixstats", handler.handlePrefixStats)
	http.HandleFunc("/", handler.handleDefault)

	// Start server
	addr := fmt.Sprintf(":%d", config.Port)
	log.Printf("Listening on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}

type BackendHandler struct {
	config Config
	prefix prefixTracker
}

// prefixTracker simulates a prefix KV cache: it remembers the canonical text
// of recent requests and reports how many leading characters of each new
// request match something already "cached" on this backend. Lets tests
// measure how well a routing mode co-locates requests that share prefixes.
type prefixTracker struct {
	mu           sync.Mutex
	texts        [][]byte
	requests     int64
	matchedChars int64
	totalChars   int64
}

// canonicalText mirrors the load balancer's canonicalization closely enough
// for prefix comparison: role and content-bearing fields per message, else
// the raw prompt, else the raw body.
func canonicalText(body []byte) []byte {
	var req struct {
		Messages []struct {
			Role             string          `json:"role"`
			Content          json.RawMessage `json:"content"`
			Reasoning        json.RawMessage `json:"reasoning"`
			ReasoningContent json.RawMessage `json:"reasoning_content"`
			ToolCalls        json.RawMessage `json:"tool_calls"`
		} `json:"messages"`
		Prompt json.RawMessage `json:"prompt"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return body
	}
	if len(req.Messages) == 0 {
		if len(req.Prompt) > 0 {
			return req.Prompt
		}
		return body
	}
	var buf bytes.Buffer
	for _, m := range req.Messages {
		buf.WriteString(m.Role)
		buf.WriteByte(0x1f)
		if m.Reasoning != nil {
			buf.Write(m.Reasoning)
		} else {
			buf.Write(m.ReasoningContent)
		}
		buf.WriteByte(0x1f)
		buf.Write(m.Content)
		buf.WriteByte(0x1f)
		buf.Write(m.ToolCalls)
		buf.WriteByte(0x1e)
	}
	return buf.Bytes()
}

func commonPrefixLen(a, b []byte) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// observe records a request and returns how many characters of its canonical
// text matched the longest prefix already stored here.
func (pt *prefixTracker) observe(body []byte) (matched, total int) {
	text := canonicalText(body)
	total = len(text)

	pt.mu.Lock()
	defer pt.mu.Unlock()
	for _, t := range pt.texts {
		if n := commonPrefixLen(t, text); n > matched {
			matched = n
		}
	}
	pt.requests++
	pt.matchedChars += int64(matched)
	pt.totalChars += int64(total)
	pt.texts = append(pt.texts, text)
	if len(pt.texts) > 1024 {
		pt.texts = pt.texts[1:]
	}
	return matched, total
}

// handlePrefixStats reports cumulative prefix-match statistics
func (h *BackendHandler) handlePrefixStats(w http.ResponseWriter, r *http.Request) {
	h.prefix.mu.Lock()
	requests := h.prefix.requests
	matched := h.prefix.matchedChars
	total := h.prefix.totalChars
	h.prefix.mu.Unlock()

	ratio := 0.0
	if total > 0 {
		ratio = float64(matched) / float64(total)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"requests":      requests,
		"matched_chars": matched,
		"total_chars":   total,
		"ratio":         ratio,
	})
}

// handleHealth handles health check requests
func (h *BackendHandler) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Determine response based on mode
	switch h.config.Mode {
	case "failing":
		log.Printf("[%d] Health check: FAILING", h.config.Port)
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return

	case "flaky":
		if rand.Float64() < h.config.FailureRate {
			log.Printf("[%d] Health check: FLAKY (failing)", h.config.Port)
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
			return
		}
		log.Printf("[%d] Health check: FLAKY (ok)", h.config.Port)

	case "timeout":
		log.Printf("[%d] Health check: TIMEOUT (sleeping forever)", h.config.Port)
		time.Sleep(1 * time.Hour)
		return

	default:
		log.Printf("[%d] Health check: OK", h.config.Port)
	}

	// Return mock /v1/models response
	response := map[string]any{
		"object": "list",
		"data": []map[string]any{
			{
				"id":       "mock-model",
				"object":   "model",
				"created":  time.Now().Unix(),
				"owned_by": "mock-backend",
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleCompletions handles completion requests
func (h *BackendHandler) handleCompletions(w http.ResponseWriter, r *http.Request) {
	log.Printf("[%d] %s %s from %s", h.config.Port, r.Method, r.URL.Path, r.RemoteAddr)

	body, _ := io.ReadAll(r.Body)
	matched, total := h.prefix.observe(body)

	// Apply randomized delay if configured
	if h.config.Delay > 0 {
		time.Sleep(time.Duration(float64(h.config.Delay) * randomFactor()))
	}

	// Determine response based on mode
	switch h.config.Mode {
	case "failing":
		log.Printf("[%d] Completions: FAILING", h.config.Port)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return

	case "flaky":
		if rand.Float64() < h.config.FailureRate {
			log.Printf("[%d] Completions: FLAKY (failing)", h.config.Port)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		log.Printf("[%d] Completions: FLAKY (ok)", h.config.Port)

	case "timeout":
		log.Printf("[%d] Completions: TIMEOUT (sleeping forever)", h.config.Port)
		time.Sleep(1 * time.Hour)
		return

	default:
		log.Printf("[%d] Completions: OK", h.config.Port)
	}

	// Generate response of randomized size
	responseText := generateText(int(float64(h.config.ResponseSize) * randomFactor()))

	response := map[string]any{
		"id":      fmt.Sprintf("cmpl-%d", time.Now().Unix()),
		"object":  "text_completion",
		"created": time.Now().Unix(),
		"model":   "mock-model",
		"choices": []map[string]any{
			{
				"text":          responseText,
				"index":         0,
				"logprobs":      nil,
				"finish_reason": "length",
			},
		},
		"usage": map[string]int{
			"prompt_tokens":     10,
			"completion_tokens": len(responseText) / 4,
			"total_tokens":      10 + len(responseText)/4,
		},
		"backend_port":         h.config.Port, // Include port to identify which backend responded
		"prefix_matched_chars": matched,
		"prefix_total_chars":   total,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// handleDefault handles all other requests
func (h *BackendHandler) handleDefault(w http.ResponseWriter, r *http.Request) {
	log.Printf("[%d] %s %s from %s", h.config.Port, r.Method, r.URL.Path, r.RemoteAddr)

	// Determine response based on mode
	switch h.config.Mode {
	case "failing":
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return

	case "flaky":
		if rand.Float64() < h.config.FailureRate {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

	case "timeout":
		time.Sleep(1 * time.Hour)
		return
	}

	response := map[string]any{
		"message":      "Mock backend server",
		"port":         h.config.Port,
		"path":         r.URL.Path,
		"method":       r.Method,
		"backend_port": h.config.Port,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// randomFactor returns a random multiplier in [0.5, 2.0].
func randomFactor() float64 {
	return 0.5 + rand.Float64()*1.5
}

// generateText generates text of approximately the specified size
func generateText(size int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 "
	result := make([]byte, size)
	for i := range result {
		result[i] = chars[rand.Intn(len(chars))]
	}
	return string(result)
}
