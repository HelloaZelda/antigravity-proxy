package pool

import (
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/HelloaZelda/antigravity-proxy/internal/auth"
	"github.com/HelloaZelda/antigravity-proxy/internal/config"
)

// AccountPool manages multiple Google accounts with health tracking and rotation
type AccountPool struct {
	mu          sync.RWMutex
	accounts    []*auth.Account
	routingMode string
	rrIndex     uint64
	client      *http.Client
}

// NewAccountPool creates an AccountPool from configuration
func NewAccountPool(cfg *config.Config, client *http.Client) *AccountPool {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}

	p := &AccountPool{
		routingMode: cfg.RoutingMode,
		client:      client,
	}

	for _, a := range cfg.Accounts {
		clientID := a.ClientID
		if clientID == "" {
			clientID = cfg.ClientID
		}
		clientSecret := a.ClientSecret
		if clientSecret == "" {
			clientSecret = cfg.ClientSecret
		}
		projectID := a.ProjectID
		if projectID == "" {
			projectID = cfg.ProjectID
		}

		p.accounts = append(p.accounts, auth.NewAccount(
			a.Email,
			a.RefreshToken,
			clientID,
			clientSecret,
			projectID,
		))
	}

	return p
}

// AccountsCount returns the number of managed accounts
func (p *AccountPool) AccountsCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.accounts)
}

// GetAccounts returns the underlying account list
func (p *AccountPool) GetAccounts() []*auth.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	list := make([]*auth.Account, len(p.accounts))
	copy(list, p.accounts)
	return list
}

// GetCandidates returns candidate accounts ordered according to the configured routing strategy
func (p *AccountPool) GetCandidates() []*auth.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var active []*auth.Account
	var cooling []*auth.Account

	for _, acc := range p.accounts {
		if acc.IsInCooldown() {
			cooling = append(cooling, acc)
		} else {
			active = append(active, acc)
		}
	}

	if len(active) == 0 {
		return cooling
	}

	switch p.routingMode {
	case "round-robin":
		idx := atomic.AddUint64(&p.rrIndex, 1)
		n := uint64(len(active))
		start := idx % n
		rotated := make([]*auth.Account, n)
		for i := uint64(0); i < n; i++ {
			rotated[i] = active[(start+i)%n]
		}
		return append(rotated, cooling...)
	default: // "priority"
		return append(active, cooling...)
	}
}

// ExecuteWithFailover executes an upstream action with multi-account rotation
func (p *AccountPool) ExecuteWithFailover(action func(acc *auth.Account, token string) (*http.Response, error)) (*http.Response, *auth.Account, error) {
	candidates := p.GetCandidates()
	if len(candidates) == 0 {
		return nil, nil, fmt.Errorf("no accounts available in pool")
	}

	var lastErr error
	for _, acc := range candidates {
		token, err := acc.GetToken(p.client, false)
		if err != nil {
			log.Printf("[AccountPool] Token error for %s: %v, trying next...", acc.Email, err)
			lastErr = err
			continue
		}

		resp, err := action(acc, token)
		if err != nil {
			log.Printf("[AccountPool] Network error on %s: %v, trying next...", acc.Email, err)
			lastErr = err
			continue
		}

		// Handle 401 Unauthorized: Force refresh once
		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			log.Printf("[AccountPool] 401 Unauthorized on %s, force-refreshing token...", acc.Email)
			token, err = acc.GetToken(p.client, true)
			if err != nil {
				lastErr = err
				continue
			}
			resp, err = action(acc, token)
			if err != nil {
				lastErr = err
				continue
			}
		}

		// Handle 429 Rate Limit
		if resp.StatusCode == http.StatusTooManyRequests {
			resp.Body.Close()
			acc.MarkCooldown(60 * time.Second)
			log.Printf("[AccountPool] 429 Rate Limit on %s, cooling down for 60s...", acc.Email)
			lastErr = fmt.Errorf("account %s rate limited (429)", acc.Email)
			continue
		}

		// Handle 403 Forbidden
		if resp.StatusCode == http.StatusForbidden {
			resp.Body.Close()
			acc.MarkCooldown(1 * time.Hour)
			log.Printf("[AccountPool] 403 Forbidden on %s, cooling down for 1h...", acc.Email)
			lastErr = fmt.Errorf("account %s forbidden (403)", acc.Email)
			continue
		}

		// Handle 5xx Server Errors
		if resp.StatusCode >= 500 {
			resp.Body.Close()
			acc.MarkCooldown(30 * time.Second)
			log.Printf("[AccountPool] Server error %d on %s, cooling down for 30s...", resp.StatusCode, acc.Email)
			lastErr = fmt.Errorf("account %s upstream server error (%d)", acc.Email, resp.StatusCode)
			continue
		}

		// Success or client error (4xx)
		return resp, acc, nil
	}

	return nil, nil, fmt.Errorf("all accounts in pool failed or exhausted: %v", lastErr)
}
