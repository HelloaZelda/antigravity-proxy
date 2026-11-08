package auth

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const TokenEndpoint = "https://oauth2.googleapis.com/token"

// Account represents an active authenticated account with auto-refreshing access token
type Account struct {
	ID           string
	Email        string
	RefreshToken string
	ClientID     string
	ClientSecret string
	ProjectID    string

	mu          sync.RWMutex
	accessToken string
	expiresAt   time.Time
	cooldownUntil time.Time
}

// NewAccount initializes an Account instance
func NewAccount(email, refreshToken, clientID, clientSecret, projectID string) *Account {
	return &Account{
		Email:        email,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		ProjectID:    projectID,
	}
}

// IsInCooldown checks whether the account is currently cooling down
func (a *Account) IsInCooldown() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return time.Now().Before(a.cooldownUntil)
}

// MarkCooldown sets a cooldown duration for this account
func (a *Account) MarkCooldown(d time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cooldownUntil = time.Now().Add(d)
}

// GetToken returns a valid access token, refreshing if necessary
func (a *Account) GetToken(client *http.Client, forceRefresh bool) (string, error) {
	a.mu.RLock()
	if !forceRefresh && a.accessToken != "" && time.Now().Before(a.expiresAt.Add(-60*time.Second)) {
		token := a.accessToken
		a.mu.RUnlock()
		return token, nil
	}
	a.mu.RUnlock()

	a.mu.Lock()
	defer a.mu.Unlock()

	// Double-check under lock
	if !forceRefresh && a.accessToken != "" && time.Now().Before(a.expiresAt.Add(-60*time.Second)) {
		return a.accessToken, nil
	}

	data := url.Values{
		"client_id":     {a.ClientID},
		"client_secret": {a.ClientSecret},
		"refresh_token": {a.RefreshToken},
		"grant_type":    {"refresh_token"},
	}

	resp, err := client.PostForm(TokenEndpoint, data)
	if err != nil {
		return "", fmt.Errorf("oauth network error: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token refresh failed for %s (%d): %s", a.Email, resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("failed to parse token response: %w", err)
	}

	a.accessToken = tokenResp.AccessToken
	a.expiresAt = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	return a.accessToken, nil
}
