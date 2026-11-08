package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

// AccountConfig defines a single Google OAuth account
type AccountConfig struct {
	ID           string `json:"id,omitempty"`
	Email        string `json:"email"`
	RefreshToken string `json:"refresh_token"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	ProjectID    string `json:"project_id,omitempty"`
}

// ProviderConfig defines an external API provider (OpenAI, DeepSeek, vLLM, etc.)
type ProviderConfig struct {
	Name         string            `json:"name"`
	BaseURL      string            `json:"base_url"`
	APIKey       string            `json:"api_key"`
	Models       []string          `json:"models"`                  // e.g. ["deepseek-chat", "Qwen3.8-Flash-Next", "qwen*"]
	ModelMapping map[string]string `json:"model_mapping,omitempty"` // e.g. {"qwen": "Qwen3.8-Flash-Next"}
	Protocol     string            `json:"protocol"`                // "openai" (default) or "anthropic"
}

// Config represents full proxy configuration
type Config struct {
	Port          int              `json:"port"`
	APIKey        string           `json:"api_key"`
	ClientID      string           `json:"client_id"`
	ClientSecret  string           `json:"client_secret"`
	RefreshToken  string           `json:"refresh_token,omitempty"` // legacy fallback
	ProjectID     string           `json:"project_id"`
	UpstreamURL   string           `json:"upstream_url"`
	RoutingMode   string           `json:"routing_mode,omitempty"` // "sticky", "priority", "round-robin"
	WebPassword   string           `json:"web_password,omitempty"`
	Accounts      []AccountConfig  `json:"accounts,omitempty"`
	Providers     []ProviderConfig `json:"providers,omitempty"`
}

// LoadConfig loads configuration from a JSON file and applies environment overrides
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", path, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config JSON: %w", err)
	}

	// Set sensible defaults
	if cfg.Port <= 0 {
		cfg.Port = 8080
	}
	if cfg.UpstreamURL == "" {
		cfg.UpstreamURL = "https://daily-cloudcode-pa.googleapis.com"
	}
	if cfg.ProjectID == "" {
		cfg.ProjectID = "aicode-consumers"
	}
	if cfg.RoutingMode == "" {
		cfg.RoutingMode = "priority"
	}

	// Environment variable overrides
	if portStr := os.Getenv("PORT"); portStr != "" {
		if p, err := strconv.Atoi(portStr); err == nil && p > 0 {
			cfg.Port = p
		}
	}
	if key := os.Getenv("API_KEY"); key != "" {
		cfg.APIKey = key
	}
	if clientID := os.Getenv("GOOGLE_CLIENT_ID"); clientID != "" {
		cfg.ClientID = clientID
	}
	if clientSecret := os.Getenv("GOOGLE_CLIENT_SECRET"); clientSecret != "" {
		cfg.ClientSecret = clientSecret
	}
	if upstream := os.Getenv("UPSTREAM_URL"); upstream != "" {
		cfg.UpstreamURL = upstream
	}
	if mode := os.Getenv("ROUTING_MODE"); mode != "" {
		cfg.RoutingMode = mode
	}

	return &cfg, nil
}

// SaveConfig persists the current configuration back to disk
func SaveConfig(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	return os.WriteFile(path, data, 0600)
}
