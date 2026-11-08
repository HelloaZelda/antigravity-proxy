package sanitize

import (
	"regexp"
	"strings"
)

var validFuncRegex = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// SanitizeFunctionName ensures function names conform to Google API requirements
func SanitizeFunctionName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "tool_func"
	}
	// Replace invalid characters like dashes or dots with underscores
	cleaned := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			return r
		}
		return '_'
	}, name)

	if !validFuncRegex.MatchString(cleaned) {
		return "tool_" + cleaned
	}
	return cleaned
}

// SanitizeParameters recursively cleans OpenAPI / JSON Schema for Google CloudCode compatibility
func SanitizeParameters(params map[string]interface{}) map[string]interface{} {
	if params == nil {
		return map[string]interface{}{
			"type": "object",
		}
	}

	result := make(map[string]interface{})
	for k, v := range params {
		// Strip unsupported metadata keys
		if k == "$schema" || k == "additionalProperties" || k == "$id" || k == "title" {
			continue
		}
		result[k] = cleanValue(v)
	}

	// Ensure top-level type is object if not specified
	if _, hasType := result["type"]; !hasType {
		result["type"] = "object"
	}

	// Validate required array against existing properties
	if reqs, ok := result["required"].([]interface{}); ok {
		props, hasProps := result["properties"].(map[string]interface{})
		var validReqs []interface{}
		for _, r := range reqs {
			reqName, isStr := r.(string)
			if isStr && hasProps {
				if _, exists := props[reqName]; exists {
					validReqs = append(validReqs, reqName)
				}
			}
		}
		if len(validReqs) > 0 {
			result["required"] = validReqs
		} else {
			delete(result, "required")
		}
	}

	return result
}

func cleanValue(v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		cleaned := make(map[string]interface{})
		for k, child := range val {
			if k == "$schema" || k == "additionalProperties" || k == "$id" || k == "title" {
				continue
			}
			cleaned[k] = cleanValue(child)
		}
		// Special handling: if type is array and items is missing, supply default string items
		if t, ok := cleaned["type"].(string); ok && t == "array" {
			if _, hasItems := cleaned["items"]; !hasItems {
				cleaned["items"] = map[string]interface{}{"type": "string"}
			}
		}
		return cleaned
	case []interface{}:
		var cleanedArr []interface{}
		for _, item := range val {
			cleanedArr = append(cleanedArr, cleanValue(item))
		}
		return cleanedArr
	default:
		return v
	}
}
