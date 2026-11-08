package sanitize

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

var imageHttpClient = &http.Client{Timeout: 12 * time.Second}

// InlineData holds image or file data for Google CloudCode
type InlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

// MessagePart represents a single multimodal part (text or image)
type MessagePart struct {
	Text       string      `json:"text,omitempty"`
	InlineData *InlineData `json:"inlineData,omitempty"`
}

// OpenAIMessage represents an incoming chat message
type OpenAIMessage struct {
	Role       string           `json:"role"`
	Content    interface{}      `json:"content"`
	Name       string           `json:"name,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
	Parts      []MessagePart    `json:"-"`
}

// OpenAIToolCall represents a tool call in assistant message
type OpenAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ExtractMessageText extracts a clean text string from string or array content blocks
func ExtractMessageText(content interface{}) string {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	if arr, ok := content.([]interface{}); ok {
		var sb strings.Builder
		for _, item := range arr {
			if s, ok := item.(string); ok {
				sb.WriteString(s)
			} else if m, ok := item.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// ExtractMessageParts extracts both text and image parts from multimodal content
func ExtractMessageParts(content interface{}) []MessagePart {
	if content == nil {
		return nil
	}

	// 1. Plain string
	if s, ok := content.(string); ok {
		if strings.TrimSpace(s) == "" {
			return []MessagePart{{Text: " "}}
		}
		return []MessagePart{{Text: s}}
	}

	// 2. Array of content blocks
	if arr, ok := content.([]interface{}); ok {
		var parts []MessagePart
		for _, item := range arr {
			if s, ok := item.(string); ok {
				if s != "" {
					parts = append(parts, MessagePart{Text: s})
				}
				continue
			}

			m, ok := item.(map[string]interface{})
			if !ok {
				continue
			}

			// Text block
			if tVal, hasText := m["text"]; hasText {
				if tStr, ok := tVal.(string); ok && tStr != "" {
					parts = append(parts, MessagePart{Text: tStr})
				}
			}

			// OpenAI image_url block
			if m["type"] == "image_url" || m["image_url"] != nil {
				imgURL := ""
				if imgMap, ok := m["image_url"].(map[string]interface{}); ok {
					imgURL, _ = imgMap["url"].(string)
				} else if s, ok := m["image_url"].(string); ok {
					imgURL = s
				}

				if imgURL != "" {
					if inline := parseImageURL(imgURL); inline != nil {
						parts = append(parts, MessagePart{InlineData: inline})
					}
				}
			}

			// Anthropic image block: {"type": "image", "source": {"type": "base64", "media_type": "...", "data": "..."}}
			if m["type"] == "image" {
				if src, ok := m["source"].(map[string]interface{}); ok {
					mType, _ := src["media_type"].(string)
					b64Data, _ := src["data"].(string)
					if mType == "" {
						mType = "image/jpeg"
					}
					if b64Data != "" {
						parts = append(parts, MessagePart{
							InlineData: &InlineData{
								MimeType: mType,
								Data:     b64Data,
							},
						})
					}
				}
			}

			// Google native inlineData block
			var iData map[string]interface{}
			if id, ok := m["inlineData"].(map[string]interface{}); ok {
				iData = id
			} else if id, ok := m["inline_data"].(map[string]interface{}); ok {
				iData = id
			}
			if iData != nil {
				mType, _ := iData["mimeType"].(string)
				if mType == "" {
					mType, _ = iData["mime_type"].(string)
				}
				b64Data, _ := iData["data"].(string)
				if mType == "" {
					mType = "image/jpeg"
				}
				if b64Data != "" {
					parts = append(parts, MessagePart{
						InlineData: &InlineData{
							MimeType: mType,
							Data:     b64Data,
						},
					})
				}
			}
		}
		return parts
	}

	return nil
}

func parseImageURL(imgURL string) *InlineData {
	imgURL = strings.TrimSpace(imgURL)
	if strings.HasPrefix(imgURL, "data:") {
		idx := strings.Index(imgURL, ";base64,")
		if idx != -1 {
			mimeType := strings.TrimPrefix(imgURL[:idx], "data:")
			b64Data := imgURL[idx+len(";base64,"):]
			if mimeType == "" {
				mimeType = "image/jpeg"
			}
			return &InlineData{
				MimeType: mimeType,
				Data:     b64Data,
			}
		}
	}

	if strings.HasPrefix(imgURL, "http://") || strings.HasPrefix(imgURL, "https://") {
		return fetchRemoteImage(imgURL)
	}

	return nil
}

func fetchRemoteImage(url string) *InlineData {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := imageHttpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return nil
	}
	defer resp.Body.Close()

	limitReader := io.LimitReader(resp.Body, 25*1024*1024)
	data, err := io.ReadAll(limitReader)
	if err != nil || len(data) == 0 {
		return nil
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" || !strings.HasPrefix(contentType, "image/") {
		contentType = http.DetectContentType(data)
	}
	if idx := strings.Index(contentType, ";"); idx != -1 {
		contentType = strings.TrimSpace(contentType[:idx])
	}
	if !strings.HasPrefix(contentType, "image/") {
		contentType = "image/jpeg"
	}

	return &InlineData{
		MimeType: contentType,
		Data:     base64.StdEncoding.EncodeToString(data),
	}
}

// SanitizeAndRepairMessages normalizes messages for Google CloudCode:
// 1. Normalizes 'developer' role to system
// 2. Extracts both text and image parts
// 3. Fixes hanging/unclosed tool calls
// 4. Merges consecutive identical roles
// 5. Guarantees no empty text parts
// 6. Guarantees the conversation does not end with a model turn
func SanitizeAndRepairMessages(msgs []OpenAIMessage) ([]string, []OpenAIMessage) {
	var systemPrompts []string
	var sanitized []OpenAIMessage

	// First pass: extract system prompts and normalize roles
	for _, m := range msgs {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		text := ExtractMessageText(m.Content)

		if role == "system" || role == "developer" {
			if strings.TrimSpace(text) != "" {
				systemPrompts = append(systemPrompts, text)
			}
			continue
		}

		if role == "assistant" {
			role = "model"
		} else if role == "tool" || role == "function" {
			role = "tool"
		} else {
			role = "user"
		}

		parts := ExtractMessageParts(m.Content)
		if len(parts) == 0 && text != "" {
			parts = []MessagePart{{Text: text}}
		}

		sanitized = append(sanitized, OpenAIMessage{
			Role:       role,
			Content:    m.Content,
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
			ToolCalls:  m.ToolCalls,
			Parts:      parts,
		})
	}

	// Second pass: Auto-close hanging tool calls
	var repaired []OpenAIMessage
	for i := 0; i < len(sanitized); i++ {
		cur := sanitized[i]
		repaired = append(repaired, cur)

		if cur.Role == "model" && len(cur.ToolCalls) > 0 {
			pendingCalls := make(map[string]OpenAIToolCall)
			for _, tc := range cur.ToolCalls {
				pendingCalls[tc.ID] = tc
			}

			j := i + 1
			for j < len(sanitized) && sanitized[j].Role == "tool" {
				delete(pendingCalls, sanitized[j].ToolCallID)
				j++
			}

			for id, tc := range pendingCalls {
				repaired = append(repaired, OpenAIMessage{
					Role:       "tool",
					ToolCallID: id,
					Name:       tc.Function.Name,
					Content:    "{\"status\": \"success\", \"message\": \"completed\"}",
					Parts:      []MessagePart{{Text: "{\"status\": \"success\", \"message\": \"completed\"}"}},
				})
			}
		}
	}

	// Third pass: Ensure dialogue does not end on a model turn
	if len(repaired) > 0 && repaired[len(repaired)-1].Role == "model" {
		repaired = append(repaired, OpenAIMessage{
			Role:    "user",
			Content: "continue",
			Parts:   []MessagePart{{Text: "continue"}},
		})
	}

	// Fourth pass: Merge consecutive turns with identical roles
	var finalMsgs []OpenAIMessage
	for _, m := range repaired {
		if len(finalMsgs) == 0 {
			finalMsgs = append(finalMsgs, m)
			continue
		}

		lastIdx := len(finalMsgs) - 1
		last := finalMsgs[lastIdx]

		if last.Role == m.Role && m.Role == "user" {
			finalMsgs[lastIdx].Parts = append(finalMsgs[lastIdx].Parts, m.Parts...)
		} else {
			finalMsgs = append(finalMsgs, m)
		}
	}

	// If empty, provide a fallback user turn
	if len(finalMsgs) == 0 {
		finalMsgs = append(finalMsgs, OpenAIMessage{
			Role:    "user",
			Content: "Hello",
			Parts:   []MessagePart{{Text: "Hello"}},
		})
	}

	return systemPrompts, finalMsgs
}

// ConvertToCloudCodeContents converts repaired messages into CloudCode contents payload
func ConvertToCloudCodeContents(msgs []OpenAIMessage, sigProvider func(callID, funcName string) string) []map[string]interface{} {
	var contents []map[string]interface{}

	for _, m := range msgs {
		if m.Role == "tool" {
			text := ExtractMessageText(m.Content)
			funcName := m.Name
			if funcName == "" {
				funcName = "tool_result"
			}
			callID := m.ToolCallID
			if callID == "" {
				callID = "call_default"
			}
			contents = append(contents, map[string]interface{}{
				"role": "user",
				"parts": []map[string]interface{}{
					{
						"functionResponse": map[string]interface{}{
							"name":     funcName,
							"id":       callID,
							"response": map[string]interface{}{"result": text},
						},
					},
				},
			})
		} else if m.Role == "model" && len(m.ToolCalls) > 0 {
			var parts []map[string]interface{}
			for _, tc := range m.ToolCalls {
				sig := sigProvider(tc.ID, tc.Function.Name)

				var args map[string]interface{}
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
				if args == nil {
					args = make(map[string]interface{})
				}

				callPart := map[string]interface{}{
					"thoughtSignature": sig,
					"functionCall": map[string]interface{}{
						"name": SanitizeFunctionName(tc.Function.Name),
						"args": args,
						"id":   tc.ID,
					},
				}
				parts = append(parts, callPart)
			}
			contents = append(contents, map[string]interface{}{
				"role":  "model",
				"parts": parts,
			})
		} else {
			var parts []map[string]interface{}

			for _, p := range m.Parts {
				if p.InlineData != nil && p.InlineData.Data != "" {
					parts = append(parts, map[string]interface{}{
						"inlineData": map[string]interface{}{
							"mimeType": p.InlineData.MimeType,
							"data":     p.InlineData.Data,
						},
					})
				} else if p.Text != "" {
					parts = append(parts, map[string]interface{}{
						"text": p.Text,
					})
				}
			}

			if len(parts) == 0 {
				parts = []map[string]interface{}{{"text": " "}}
			}

			role := m.Role
			if role == "assistant" {
				role = "model"
			}
			contents = append(contents, map[string]interface{}{
				"role":  role,
				"parts": parts,
			})
		}
	}

	return contents
}
