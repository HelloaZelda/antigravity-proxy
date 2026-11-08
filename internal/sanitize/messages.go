package sanitize

import (
	"encoding/json"
	"strings"
)

// OpenAIMessage represents an incoming chat message
type OpenAIMessage struct {
	Role       string          `json:"role"`
	Content    interface{}     `json:"content"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
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

// ExtractMessageText extracts a clean string from string or array content blocks
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
			if m, ok := item.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// SanitizeAndRepairMessages normalizes messages for Google CloudCode:
// 1. Normalizes 'developer' role to system
// 2. Fixes hanging/unclosed tool calls
// 3. Merges consecutive identical roles
// 4. Guarantees no empty text parts
// 5. Guarantees the conversation does not end with a model turn
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

		sanitized = append(sanitized, OpenAIMessage{
			Role:       role,
			Content:    text,
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
			ToolCalls:  m.ToolCalls,
		})
	}

	// Second pass: Auto-close hanging tool calls
	// If a model message has ToolCalls, the immediate following messages MUST be tool responses
	var repaired []OpenAIMessage
	for i := 0; i < len(sanitized); i++ {
		cur := sanitized[i]
		repaired = append(repaired, cur)

		if cur.Role == "model" && len(cur.ToolCalls) > 0 {
			// Find all call IDs that must have tool answers
			pendingCalls := make(map[string]OpenAIToolCall)
			for _, tc := range cur.ToolCalls {
				pendingCalls[tc.ID] = tc
			}

			// Scan forward to see which calls have answers
			j := i + 1
			for j < len(sanitized) && sanitized[j].Role == "tool" {
				delete(pendingCalls, sanitized[j].ToolCallID)
				j++
			}

			// If next message is a user message or end-of-list, but some calls are still unanswered:
			for id, tc := range pendingCalls {
				repaired = append(repaired, OpenAIMessage{
					Role:       "tool",
					ToolCallID: id,
					Name:       tc.Function.Name,
					Content:    "{\"status\": \"success\", \"message\": \"completed\"}",
				})
			}
		}
	}

	// Third pass: Ensure dialogue does not end on a model turn
	if len(repaired) > 0 && repaired[len(repaired)-1].Role == "model" {
		repaired = append(repaired, OpenAIMessage{
			Role:    "user",
			Content: "continue",
		})
	}

	// Fourth pass: Merge consecutive turns with identical roles (excluding tool messages)
	var finalMsgs []OpenAIMessage
	for _, m := range repaired {
		if len(finalMsgs) == 0 {
			finalMsgs = append(finalMsgs, m)
			continue
		}

		lastIdx := len(finalMsgs) - 1
		last := finalMsgs[lastIdx]

		// Only merge user with user, or model with model (when neither has tool calls)
		if last.Role == m.Role && m.Role == "user" {
			mergedText := ExtractMessageText(last.Content) + "\n\n" + ExtractMessageText(m.Content)
			finalMsgs[lastIdx].Content = mergedText
		} else {
			finalMsgs = append(finalMsgs, m)
		}
	}

	// If empty, provide a fallback user turn
	if len(finalMsgs) == 0 {
		finalMsgs = append(finalMsgs, OpenAIMessage{
			Role:    "user",
			Content: "Hello",
		})
	}

	return systemPrompts, finalMsgs
}

// ConvertToCloudCodeContents converts repaired messages into CloudCode contents payload
func ConvertToCloudCodeContents(msgs []OpenAIMessage, sigProvider func(callID, funcName string) string) []map[string]interface{} {
	var contents []map[string]interface{}

	for _, m := range msgs {
		text := ExtractMessageText(m.Content)

		if m.Role == "tool" {
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
			if strings.TrimSpace(text) == "" {
				text = " "
			}
			role := m.Role
			if role == "assistant" {
				role = "model"
			}
			contents = append(contents, map[string]interface{}{
				"role":  role,
				"parts": []map[string]interface{}{{"text": text}},
			})
		}
	}

	return contents
}
