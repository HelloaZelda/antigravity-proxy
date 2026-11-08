package adapter

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/HelloaZelda/antigravity-proxy/internal/auth"
	"github.com/HelloaZelda/antigravity-proxy/internal/pool"
	"github.com/HelloaZelda/antigravity-proxy/internal/router"
)

// GeminiHandler handles native Google Gemini REST API endpoints
type GeminiHandler struct {
	pool        *pool.AccountPool
	upstreamURL string
	router      *router.Router
}

// NewGeminiHandler creates a new GeminiHandler
func NewGeminiHandler(p *pool.AccountPool, upstream string, r *router.Router) *GeminiHandler {
	return &GeminiHandler{
		pool:        p,
		upstreamURL: upstream,
		router:      r,
	}
}

// HandleGenerateContent forwards native /v1beta/models/... calls
func (h *GeminiHandler) HandleGenerateContent(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request", http.StatusBadRequest)
		return
	}

	// Extract model from URL path e.g. /v1beta/models/gemini-2.5-flash:generateContent
	path := r.URL.Path
	model := "gemini-3.8-flash-high"
	if strings.Contains(path, "/models/") {
		parts := strings.Split(path, "/models/")
		if len(parts) > 1 {
			mPart := parts[1]
			if idx := strings.Index(mPart, ":"); idx != -1 {
				model = mPart[:idx]
			} else {
				model = mPart
			}
		}
	}

	upstreamModel := router.MapModel(model)
	if upstreamModel == "" {
		upstreamModel = "gemini-3.8-flash-high"
	}

	isStream := strings.Contains(path, "streamGenerateContent")

	upResp, activeAccount, err := h.pool.ExecuteWithFailover(func(acc *auth.Account, token string) (*http.Response, error) {
		upstreamURL := fmt.Sprintf("%s/v1internal:streamGenerateContent?alt=sse", h.upstreamURL)
		
		wrappedPayload := fmt.Sprintf(`{"project":"%s","model":"%s","request":%s}`, acc.ProjectID, upstreamModel, string(bodyBytes))

		upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader([]byte(wrappedPayload)))
		if err != nil {
			return nil, err
		}
		upReq.Header.Set("Content-Type", "application/json")
		upReq.Header.Set("Authorization", "Bearer "+token)
		upReq.Header.Set("User-Agent", "antigravity/hub/2.17.0 (aidev_client; os_type=linux; arch=amd64; cl=986210228)")

		return http.DefaultClient.Do(upReq)
	})

	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"Upstream error: %v"}}`, err), http.StatusBadGateway)
		return
	}
	defer upResp.Body.Close()

	if upResp.StatusCode != http.StatusOK {
		respErr, _ := io.ReadAll(upResp.Body)
		log.Printf("[Error] Gemini Native Upstream %d on %s: %s", upResp.StatusCode, activeAccount.Email, string(respErr))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(upResp.StatusCode)
		w.Write(respErr)
		return
	}

	for k, v := range upResp.Header {
		w.Header()[k] = v
	}
	if isStream {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(upResp.StatusCode)
	io.Copy(w, upResp.Body)
}
