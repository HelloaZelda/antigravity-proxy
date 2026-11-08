package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"time"
)

const (
	AuthEndpoint = "https://accounts.google.com/o/oauth2/v2/auth"
	OAuthScope   = "https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email openid"
)

// LoginResult contains tokens obtained from interactive login
type LoginResult struct {
	Email        string
	RefreshToken string
	AccessToken  string
	ProjectID    string
}

// InteractiveLogin starts a local server on port 18888 and directs user to authenticate
func InteractiveLogin(clientID, clientSecret string) (*LoginResult, error) {
	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("client_id and client_secret are required for OAuth login")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:18888")
	if err != nil {
		return nil, fmt.Errorf("failed to bind local callback port 18888: %w", err)
	}
	defer listener.Close()

	redirectURI := "http://127.0.0.1:18888/callback"
	authURL := fmt.Sprintf("%s?client_id=%s&redirect_uri=%s&response_type=code&scope=%s&access_type=offline&prompt=consent",
		AuthEndpoint,
		url.QueryEscape(clientID),
		url.QueryEscape(redirectURI),
		url.QueryEscape(OAuthScope),
	)

	fmt.Println("\n=======================================================")
	fmt.Println("🚀 Antigravity-Proxy Interactive OAuth 2.0 Login")
	fmt.Println("=======================================================")
	fmt.Printf("Please open the following authorization URL in your browser:\n\n%s\n\n", authURL)

	// Try auto-opening default browser
	openBrowser(authURL)

	codeChan := make(chan string)
	errChan := make(chan error)

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/callback" {
				http.NotFound(w, r)
				return
			}
			code := r.URL.Query().Get("code")
			if code == "" {
				errMsg := r.URL.Query().Get("error")
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, "<html><body><h2>❌ Authorization Failed: %s</h2></body></html>", errMsg)
				errChan <- fmt.Errorf("authorization failed: %s", errMsg)
				return
			}

			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, "<html><body><h2 style='color:#10b981;'>✅ Authorization Successful!</h2><p>You can now close this tab and return to your terminal.</p></body></html>")
			codeChan <- code
		}),
	}

	go func() {
		_ = server.Serve(listener)
	}()

	var authCode string
	select {
	case code := <-codeChan:
		authCode = code
	case err := <-errChan:
		return nil, err
	case <-time.After(3 * time.Minute):
		return nil, fmt.Errorf("login timed out after 3 minutes")
	}

	_ = server.Shutdown(context.Background())

	// Exchange code for refresh_token
	data := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {authCode},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirectURI},
	}

	resp, err := http.PostForm(TokenEndpoint, data)
	if err != nil {
		return nil, fmt.Errorf("failed to exchange auth code: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed (%d): %s", resp.StatusCode, string(body))
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", err)
	}

	if tokenResp.RefreshToken == "" {
		return nil, fmt.Errorf("Google did not return a refresh_token (try removing app access from Google Account Security settings to force re-consent)")
	}

	// Fetch user email using access_token
	req, _ := http.NewRequest("GET", "https://www.googleapis.com/oauth2/v2/userinfo", nil)
	req.Header.Set("Authorization", "Bearer "+tokenResp.AccessToken)
	userResp, err := http.DefaultClient.Do(req)
	email := "authenticated_user"
	if err == nil && userResp.StatusCode == http.StatusOK {
		var userInfo struct {
			Email string `json:"email"`
		}
		userBody, _ := io.ReadAll(userResp.Body)
		userResp.Body.Close()
		if err := json.Unmarshal(userBody, &userInfo); err == nil && userInfo.Email != "" {
			email = userInfo.Email
		}
	}

	return &LoginResult{
		Email:        email,
		RefreshToken: tokenResp.RefreshToken,
		AccessToken:  tokenResp.AccessToken,
		ProjectID:    "aicode-consumers",
	}, nil
}

func openBrowser(url string) {
	var cmd string
	var args []string

	switch runtime.GOOS {
	case "windows":
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", url}
	case "darwin":
		cmd = "open"
		args = []string{url}
	default: // "linux", "freebsd", "openbsd", "netbsd"
		cmd = "xdg-open"
		args = []string{url}
	}
	_ = exec.Command(cmd, args...).Start()
}
