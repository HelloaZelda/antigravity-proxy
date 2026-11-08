package adapter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/HelloaZelda/antigravity-proxy/internal/auth"
	"github.com/HelloaZelda/antigravity-proxy/internal/cache"
	"github.com/HelloaZelda/antigravity-proxy/internal/pool"
	"github.com/HelloaZelda/antigravity-proxy/internal/router"
	"github.com/HelloaZelda/antigravity-proxy/internal/sanitize"
)

// AnthropicHandler processes Anthropic /v1/messages requests
type AnthropicHandler struct {
	pool        *pool.AccountPool
	sigCache    *cache.SignatureCache
	upstreamURL string
	router      *router.Router
}

// NewAnthropicHandler creates a new AnthropicHandler
func NewAnthropicHandler(p *pool.AccountPool, sc *cache.SignatureCache, upstream string, r *router.Router) *AnthropicHandler {
	return &AnthropicHandler{
		pool:        p,
		sigCache:    sc,
		upstreamURL: upstream,
		router:      r,
	}
}

type AnthropicRequest struct {
	Model       string          `json:"model"`
	Messages    json.RawMessage `json:"messages"`
	System      interface{}     `json:"system,omitempty"`
	Stream      bool            `json:"stream"`
	Temperature *float64        `json:"temperature,omitempty"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	Tools       json.RawMessage `json:"tools,omitempty"`
}

// HandleMessages handles Anthropic /v1/messages endpoint
func (h *AnthropicHandler) HandleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":{"message":"Failed to read request body"}}`, http.StatusBadRequest)
		return
	}

	var req AnthropicRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"Invalid JSON: %v"}}`, err), http.StatusBadRequest)
		return
	}

	// 1. External provider check
	if extProvider := h.router.FindProvider(req.Model); extProvider != nil {
		h.router.ForwardToExternal(w, r, extProvider, "/messages", bodyBytes)
		return
	}

	// 2. Parse Anthropic messages into standard OpenAIMessage format
	var rawMsgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal(req.Messages, &rawMsgs)

	var oaiMsgs []sanitize.OpenAIMessage

	// Extract system prompts
	if req.System != nil {
		if sysStr, ok := req.System.(string); ok && strings.TrimSpace(sysStr) != "" {
			oaiMsgs = append(oaiMsgs, sanitize.OpenAIMessage{Role: "system", Content: sysStr})
		} else if arr, ok := req.System.([]interface{}); ok {
			var sb strings.Builder
			for _, item := range arr {
				if m, ok := item.(map[string]interface{}); ok {
					if t, ok := m["text"].(string); ok {
						sb.WriteString(t + "\n")
					}
				}
			}
			if sb.Len() > 0 {
				oaiMsgs = append(oaiMsgs, sanitize.OpenAIMessage{Role: "system", Content: sb.String()})
			}
		}
	}

	// Convert content blocks
	for _, rm := range rawMsgs {
		var contentStr string
		if err := json.Unmarshal(rm.Content, &contentStr); err == nil {
			oaiMsgs = append(oaiMsgs, sanitize.OpenAIMessage{Role: rm.Role, Content: contentStr})
			continue
		}

		var blocks []map[string]interface{}
		if err := json.Unmarshal(rm.Content, &blocks); err == nil {
			var textParts []string
			var toolCalls []sanitize.OpenAIToolCall

			for _, b := range blocks {
				bType, _ := b["type"].(string)
				switch bType {
				case "text":
					if t, ok := b["text"].(string); ok {
						textParts = append(textParts, t)
					}
				case "tool_use":
					id, _ := b["id"].(string)
					name, _ := b["name"].(string)
					argsBytes, _ := json.Marshal(b["input"])
					toolCalls = append(toolCalls, sanitize.OpenAIToolCall{
						ID:   id,
						Type: "function",
						Function: struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						}{
							Name:      name,
							Arguments: string(argsBytes),
						},
					})
				case "tool_result":
					callID, _ := b["tool_use_id"].(string)
					contentVal := b["content"]
					var resText string
					if cs, ok := contentVal.(string); ok {
						resText = cs
					} else {
						cb, _ := json.Marshal(contentVal)
						resText = string(cb)
					}
					oaiMsgs = append(oaiMsgs, sanitize.OpenAIMessage{
						Role:       "tool",
						ToolCallID: callID,
						Content:    resText,
					})
				}
			}

			if len(textParts) > 0 || len(toolCalls) > 0 {
				oaiMsgs = append(oaiMsgs, sanitize.OpenAIMessage{
					Role:      rm.Role,
					Content:   strings.Join(textParts, "\n"),
					ToolCalls: toolCalls,
				})
			}
		}
	}

	// 3. Map model
	upstreamModel := router.MapModel(req.Model)
	if upstreamModel == "" {
		upstreamModel = "claude-sonnet-4-6"
	}

	// 4. Build sanitized CloudCode payload
	buildPayload := func(acc *auth.Account) map[string]interface{} {
		systemPrompts, cleanMsgs := sanitize.SanitizeAndRepairMessages(oaiMsgs)
		contents := sanitize.ConvertToCloudCodeContents(cleanMsgs, func(callID, funcName string) string {
			sig := h.sigCache.Get(callID)
			if sig == cache.DefaultFallbackSignature && funcName != "" {
				sig = h.sigCache.Get(funcName)
			}
			return sig
		})

		genConfig := map[string]interface{}{
			"thinkingConfig": map[string]interface{}{
				"includeThoughts": true,
			},
			"maxOutputTokens": sanitize.ClampMaxTokens(req.MaxTokens, 65536),
		}
		if clampedTemp := sanitize.ClampTemperature(req.Temperature); clampedTemp != nil {
			genConfig["temperature"] = *clampedTemp
		}

		reqMap := map[string]interface{}{
			"contents":         contents,
			"generationConfig": genConfig,
		}

		if len(systemPrompts) > 0 {
			var sysParts []map[string]interface{}
			for _, sp := range systemPrompts {
				sysParts = append(sysParts, map[string]interface{}{"text": sp})
			}
			reqMap["systemInstruction"] = map[string]interface{}{
				"role":  "user",
				"parts": sysParts,
			}
		}

		// Anthropic tools conversion
		if len(req.Tools) > 0 && string(req.Tools) != "null" && string(req.Tools) != "[]" {
			var anthTools []struct {
				Name        string                 `json:"name"`
				Description string                 `json:"description"`
				InputSchema map[string]interface{} `json:"input_schema"`
			}
			if err := json.Unmarshal(req.Tools, &anthTools); err == nil && len(anthTools) > 0 {
				var decls []map[string]interface{}
				for _, t := range anthTools {
					decls = append(decls, map[string]interface{}{
						"name":        sanitize.SanitizeFunctionName(t.Name),
						"description": t.Description,
						"parameters":  sanitize.SanitizeParameters(t.InputSchema),
					})
				}
				if len(decls) > 0 {
					reqMap["tools"] = []map[string]interface{}{
						{"functionDeclarations": decls},
					}
				}
			}
		}

		return map[string]interface{}{
			"project": acc.ProjectID,
			"model":   upstreamModel,
			"request": reqMap,
		}
	}

	// 5. Execute request with failover pool
	upResp, activeAccount, err := h.pool.ExecuteWithFailover(func(acc *auth.Account, token string) (*http.Response, error) {
		payload := buildPayload(acc)
		data, _ := json.Marshal(payload)

		url := fmt.Sprintf("%s/v1internal:streamGenerateContent?alt=sse", h.upstreamURL)
		upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		upReq.Header.Set("Content-Type", "application/json")
		upReq.Header.Set("Authorization", "Bearer "+token)
		upReq.Header.Set("User-Agent", "antigravity/hub/2.17.0 (aidev_client; os_type=linux; arch=amd64; cl=986210228)")

		return http.DefaultClient.Do(upReq)
	})

	if err != nil {
		log.Printf("[Error] Upstream pool failed: %v", err)
		http.Error(w, fmt.Sprintf(`{"error":{"message":"Upstream error: %v"}}`, err), http.StatusBadGateway)
		return
	}
	defer upResp.Body.Close()

	if upResp.StatusCode != http.StatusOK {
		respErr, _ := io.ReadAll(upResp.Body)
		log.Printf("[Error] Upstream %d on %s: %s | Anthropic Model: %s -> %s", upResp.StatusCode, activeAccount.Email, string(respErr), req.Model, upstreamModel)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(upResp.StatusCode)
		w.Write(respErr)
		return
	}

	msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())

	// 6. Streaming output in Anthropic format
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
			return
		}

		sendEvent := func(eventType string, data interface{}) {
			dBytes, _ := json.Marshal(data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, dBytes)
			flusher.Flush()
		}

		sendEvent("message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":           msgID,
				"type":         "message",
				"role":         "assistant",
				"model":        req.Model,
				"content":      []interface{}{},
				"stop_reason":  nil,
				"stop_sequence": nil,
				"usage":        map[string]int{"input_tokens": 10, "output_tokens": 1},
			},
		})

		sendEvent("content_block_start", map[string]interface{}{
			"type":          "content_block_start",
			"index":         0,
			"content_block": map[string]interface{}{"type": "text", "text": ""},
		})

		scanner := bufio.NewScanner(upResp.Body)
		buf := make([]byte, 1024*1024)
		scanner.Buffer(buf, 10*1024*1024)

		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			dataStr := strings.TrimPrefix(line, "data: ")
			if strings.TrimSpace(dataStr) == "" {
				continue
			}

			var chunk CloudCodeChunk
			if err := json.Unmarshal([]byte(dataStr), &chunk); err != nil {
				continue
			}

			for _, cand := range chunk.Response.Candidates {
				for _, part := range cand.Content.Parts {
					if part.Text != "" {
						sendEvent("content_block_delta", map[string]interface{}{
							"type":  "content_block_delta",
							"index": 0,
							"delta": map[string]interface{}{"type": "text_delta", "text": part.Text},
						})
					}
				}
			}
		}

		sendEvent("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 0})
		sendEvent("message_delta", map[string]interface{}{
			"type":  "message_delta",
			"delta": map[string]interface{}{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]int{"output_tokens": 50},
		})
		sendEvent("message_stop", map[string]interface{}{"type": "message_stop"})
		return
	}

	// 7. Non-streaming output in Anthropic format
	var fullText strings.Builder
	scanner := bufio.NewScanner(upResp.Body)
	buf := make([]byte, 1024*1024)
	scanner.Buffer(buf, 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		dataStr := strings.TrimPrefix(line, "data: ")
		if strings.TrimSpace(dataStr) == "" {
			continue
		}
		var chunk CloudCodeChunk
		if err := json.Unmarshal([]byte(dataStr), &chunk); err != nil {
			continue
		}
		for _, cand := range chunk.Response.Candidates {
			for _, part := range cand.Content.Parts {
				if part.Text != "" {
					fullText.WriteString(part.Text)
				}
			}
		}
	}

	respObj := map[string]interface{}{
		"id":           msgID,
		"type":         "message",
		"role":         "assistant",
		"model":        req.Model,
		"stop_reason":  "end_turn",
		"stop_sequence": nil,
		"content": []map[string]interface{}{
			{
				"type": "text",
				"text": fullText.String(),
			},
		},
		"usage": map[string]int{
			"input_tokens":  20,
			"output_tokens": len(fullText.String()) / 4,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(respObj)
}
