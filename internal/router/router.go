package router

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/HelloaZelda/antigravity-proxy/internal/config"
)

// MapModel maps client model names to real Google CloudCode backend models
func MapModel(m string) string {
	lower := strings.ToLower(strings.TrimSpace(m))
	switch {
	// OpenAI mappings
	case strings.Contains(lower, "gpt-4o-mini"):
		return "gemini-3.8-flash-medium"
	case strings.Contains(lower, "gpt-4o"):
		return "gemini-3.8-flash-high"

	// Claude mappings
	case strings.Contains(lower, "claude-3-7") || strings.Contains(lower, "claude-3.7"):
		return "claude-sonnet-4-6"
	case strings.Contains(lower, "claude-3-5") || strings.Contains(lower, "claude-3.5"):
		return "claude-sonnet-4-6"
	case strings.Contains(lower, "claude-opus"):
		return "claude-opus-4-6-thinking"
	case strings.Contains(lower, "claude-sonnet"):
		return "claude-sonnet-4-6"

	// Gemini family mappings
	case strings.EqualFold(lower, "gemini-3.8-thinking"):
		return "gemini-3.8-flash-high"
	case strings.EqualFold(lower, "gemini-3.8-flash"):
		return "gemini-3.8-flash-high"
	case strings.EqualFold(lower, "gemini-3.7-flash"):
		return "gemini-3.8-flash-high"
	case strings.EqualFold(lower, "gemini-3.1-pro") || strings.EqualFold(lower, "gemini-3.1-pro-preview"):
		return "gemini-3.8-flash-high"
	case strings.EqualFold(lower, "gemini-2.0-flash") || strings.EqualFold(lower, "gemini-1.5-pro"):
		return "gemini-2.5-flash"

	default:
		return m
	}
}

// Router handles provider matching and reverse proxy forwarding
type Router struct {
	providers []config.ProviderConfig
	client    *http.Client
}

// NewRouter initializes Router with providers
func NewRouter(providers []config.ProviderConfig, client *http.Client) *Router {
	if client == nil {
		client = http.DefaultClient
	}
	return &Router{
		providers: providers,
		client:    client,
	}
}

// FindProvider finds an external provider configured for the given model
func (r *Router) FindProvider(model string) *config.ProviderConfig {
	for _, p := range r.providers {
		for _, pattern := range p.Models {
			if strings.EqualFold(pattern, model) {
				return &p
			}
			if matched, _ := filepath.Match(pattern, model); matched {
				return &p
			}
		}
	}
	return nil
}

// ForwardToExternal forwards request to an external provider (OpenAI, DeepSeek, vLLM)
func (r *Router) ForwardToExternal(w http.ResponseWriter, req *http.Request, p *config.ProviderConfig, path string, body []byte) {
	targetURL := strings.TrimRight(p.BaseURL, "/") + path
	outReq, err := http.NewRequestWithContext(req.Context(), req.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	for k, v := range req.Header {
		outReq.Header[k] = v
	}
	outReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	outReq.Header.Set("x-api-key", p.APIKey)
	outReq.Host = outReq.URL.Host

	resp, err := r.client.Do(outReq)
	if err != nil {
		log.Printf("[Router] Error forwarding to %s: %v", p.Name, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)

	if flusher, ok := w.(http.Flusher); ok {
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				flusher.Flush()
			}
			if err != nil {
				break
			}
		}
	} else {
		io.Copy(w, resp.Body)
	}
}
