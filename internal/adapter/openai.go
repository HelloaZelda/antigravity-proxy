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

// OpenAIHandler processes OpenAI compatible /v1/chat/completions requests
type OpenAIHandler struct {
	pool        *pool.AccountPool
	sigCache    *cache.SignatureCache
	upstreamURL string
	router      *router.Router
}

// NewOpenAIHandler creates a new OpenAIHandler
func NewOpenAIHandler(p *pool.AccountPool, sc *cache.SignatureCache, upstream string, r *router.Router) *OpenAIHandler {
	return &OpenAIHandler{
		pool:        p,
		sigCache:    sc,
		upstreamURL: upstream,
		router:      r,
	}
}

type OpenAIRequest struct {
	Model       string                   `json:"model"`
	Messages    []sanitize.OpenAIMessage `json:"messages"`
	Stream      bool                     `json:"stream"`
	Temperature *float64                 `json:"temperature,omitempty"`
	MaxTokens   *int                     `json:"max_tokens,omitempty"`
	TopP        *float64                 `json:"top_p,omitempty"`
	Tools       json.RawMessage          `json:"tools,omitempty"`
}

type CloudCodeChunk struct {
	Response struct {
		Candidates []struct {
			Content struct {
				Role  string `json:"role"`
				Parts []struct {
					Text             string `json:"text,omitempty"`
					ThoughtSignature string `json:"thoughtSignature,omitempty"`
					FunctionCall     *struct {
						Name string                 `json:"name"`
						Args map[string]interface{} `json:"args"`
						ID   string                 `json:"id"`
					} `json:"functionCall,omitempty"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	} `json:"response"`
}

// HandleChatCompletions handles OpenAI chat completions
func (h *OpenAIHandler) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":{"message":"Failed to read request body"}}`, http.StatusBadRequest)
		return
	}

	var req OpenAIRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"Invalid JSON: %v"}}`, err), http.StatusBadRequest)
		return
	}

	// 1. External provider check
	if extProvider := h.router.FindProvider(req.Model); extProvider != nil {
		log.Printf("[Router] Forwarding %s to provider %s", req.Model, extProvider.Name)
		sendBytes := bodyBytes
		targetModel := ""
		if extProvider.ModelMapping != nil {
			for k, v := range extProvider.ModelMapping {
				if strings.EqualFold(k, req.Model) {
					targetModel = v
					break
				}
			}
		}
		if targetModel != "" && targetModel != req.Model {
			var rawMap map[string]interface{}
			if err := json.Unmarshal(bodyBytes, &rawMap); err == nil {
				rawMap["model"] = targetModel
				sendBytes, _ = json.Marshal(rawMap)
			}
		}
		h.router.ForwardToExternal(w, r, extProvider, "/chat/completions", sendBytes)
		return
	}

	// 2. Map model for Google CloudCode
	upstreamModel := router.MapModel(req.Model)
	if upstreamModel == "" {
		upstreamModel = "gemini-3.8-flash-high"
	}

	// 3. Build sanitized CloudCode payload
	buildPayload := func(acc *auth.Account) map[string]interface{} {
		systemPrompts, cleanMsgs := sanitize.SanitizeAndRepairMessages(req.Messages)
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
		if clampedTopP := sanitize.ClampTopP(req.TopP); clampedTopP != nil {
			genConfig["topP"] = *clampedTopP
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

		// Sanitize and attach tools
		if len(req.Tools) > 0 && string(req.Tools) != "null" && string(req.Tools) != "[]" {
			var oaiTools []struct {
				Type     string                 `json:"type"`
				Function struct {
					Name        string                 `json:"name"`
					Description string                 `json:"description"`
					Parameters  map[string]interface{} `json:"parameters"`
				} `json:"function"`
			}
			if err := json.Unmarshal(req.Tools, &oaiTools); err == nil && len(oaiTools) > 0 {
				var decls []map[string]interface{}
				for _, t := range oaiTools {
					if t.Type == "function" || t.Function.Name != "" {
						decls = append(decls, map[string]interface{}{
							"name":        sanitize.SanitizeFunctionName(t.Function.Name),
							"description": t.Function.Description,
							"parameters":  sanitize.SanitizeParameters(t.Function.Parameters),
						})
					}
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

	// 4. Execute request with failover pool
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
		log.Printf("[Error] Upstream %d on %s: %s | Model: %s -> %s", upResp.StatusCode, activeAccount.Email, string(respErr), req.Model, upstreamModel)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(upResp.StatusCode)
		w.Write(respErr)
		return
	}

	reqID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	// 5. Streaming output
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
			return
		}

		scanner := bufio.NewScanner(upResp.Body)
		buf := make([]byte, 1024*1024)
		scanner.Buffer(buf, 10*1024*1024)

		toolCallIndex := 0

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
					// Cache thought signature
					if part.ThoughtSignature != "" && part.FunctionCall != nil {
						cid := part.FunctionCall.ID
						if cid == "" {
							cid = fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), toolCallIndex)
						}
						h.sigCache.Set(cid, part.ThoughtSignature)
						h.sigCache.Set(part.FunctionCall.Name, part.ThoughtSignature)
					}

					// Output function call chunk
					if part.FunctionCall != nil {
						cid := part.FunctionCall.ID
						if cid == "" {
							cid = fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), toolCallIndex)
						}
						argsBytes, _ := json.Marshal(part.FunctionCall.Args)

						chunkResp := map[string]interface{}{
							"id":      reqID,
							"object":  "chat.completion.chunk",
							"created": created,
							"model":   req.Model,
							"choices": []map[string]interface{}{
								{
									"index": 0,
									"delta": map[string]interface{}{
										"tool_calls": []map[string]interface{}{
											{
												"index": toolCallIndex,
												"id":    cid,
												"type":  "function",
												"function": map[string]interface{}{
													"name":      part.FunctionCall.Name,
													"arguments": string(argsBytes),
												},
											},
										},
									},
								},
							},
						}
						toolCallIndex++
						outBytes, _ := json.Marshal(chunkResp)
						fmt.Fprintf(w, "data: %s\n\n", outBytes)
						flusher.Flush()
					}

					// Output text content chunk
					if part.Text != "" {
						chunkResp := map[string]interface{}{
							"id":      reqID,
							"object":  "chat.completion.chunk",
							"created": created,
							"model":   req.Model,
							"choices": []map[string]interface{}{
								{
									"index": 0,
									"delta": map[string]interface{}{
										"content": part.Text,
									},
								},
							},
						}
						outBytes, _ := json.Marshal(chunkResp)
						fmt.Fprintf(w, "data: %s\n\n", outBytes)
						flusher.Flush()
					}
				}
			}
		}

		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	// 6. Non-streaming output
	var fullText strings.Builder
	var toolCalls []map[string]interface{}
	toolIndex := 0

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
				if part.ThoughtSignature != "" && part.FunctionCall != nil {
					cid := part.FunctionCall.ID
					if cid == "" {
						cid = fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), toolIndex)
					}
					h.sigCache.Set(cid, part.ThoughtSignature)
					h.sigCache.Set(part.FunctionCall.Name, part.ThoughtSignature)
				}

				if part.FunctionCall != nil {
					cid := part.FunctionCall.ID
					if cid == "" {
						cid = fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), toolIndex)
					}
					argsBytes, _ := json.Marshal(part.FunctionCall.Args)
					toolCalls = append(toolCalls, map[string]interface{}{
						"id":   cid,
						"type": "function",
						"function": map[string]interface{}{
							"name":      part.FunctionCall.Name,
							"arguments": string(argsBytes),
						},
					})
					toolIndex++
				}

				if part.Text != "" {
					fullText.WriteString(part.Text)
				}
			}
		}
	}

	msgObj := map[string]interface{}{
		"role":    "assistant",
		"content": fullText.String(),
	}
	if len(toolCalls) > 0 {
		msgObj["tool_calls"] = toolCalls
	}

	respObj := map[string]interface{}{
		"id":      reqID,
		"object":  "chat.completion",
		"created": created,
		"model":   req.Model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"message":       msgObj,
				"finish_reason": "stop",
			},
		},
		"usage": map[string]interface{}{
			"prompt_tokens":     len(req.Messages) * 10,
			"completion_tokens": len(fullText.String()) / 4,
			"total_tokens":      len(req.Messages)*10 + len(fullText.String())/4,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(respObj)
}

// HandleModels returns the available models list in standard OpenAI format
func (h *OpenAIHandler) HandleModels(w http.ResponseWriter, r *http.Request) {
	models := []string{
		"gemini-3.8-flash-high",
		"gemini-3.8-flash-medium",
		"gemini-3.8-flash-low",
		"gemini-3.8-thinking",
		"gemini-2.5-flash",
		"claude-sonnet-4-6",
		"claude-opus-4-6-thinking",
		"claude-3-7-sonnet",
		"claude-3-5-sonnet",
		"gpt-4o",
		"gpt-4o-mini",
	}

	var data []map[string]interface{}
	for _, m := range models {
		data = append(data, map[string]interface{}{
			"id":       m,
			"object":   "model",
			"created":  1700000000,
			"owned_by": "antigravity",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}
