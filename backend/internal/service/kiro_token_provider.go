package service

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
)

func KiroTokenCacheKey(account *Account) string {
	return "kiro:account:" + strconv.FormatInt(account.ID, 10)
}

type kiroAccountClient struct {
	upstream HTTPUpstream
	account  *Account
}

func (c kiroAccountClient) Do(req *http.Request) (*http.Response, error) {
	proxy := ""
	if c.account.ProxyID != nil && c.account.Proxy != nil {
		proxy = c.account.Proxy.URL()
	}
	return c.upstream.Do(req, proxy, c.account.ID, c.account.Concurrency)
}

type KiroTokenRefresher struct {
	upstream      HTTPUpstream
	force         bool
	rejectedToken string
}

func (r *KiroTokenRefresher) CacheKey(a *Account) string { return KiroTokenCacheKey(a) }
func (r *KiroTokenRefresher) CanRefresh(a *Account) bool {
	return a != nil && a.Platform == PlatformKiro && a.Type == AccountTypeOAuth
}
func (r *KiroTokenRefresher) NeedsRefresh(a *Account, window time.Duration) bool {
	if !r.CanRefresh(a) {
		return false
	}
	if r.force {
		return r.rejectedToken == "" || a.GetCredential("access_token") == r.rejectedToken
	}
	exp := a.GetCredentialAsTime("expires_at")
	return a.GetCredential("access_token") == "" || exp == nil || time.Until(*exp) <= window
}
func (r *KiroTokenRefresher) Refresh(ctx context.Context, a *Account) (map[string]any, error) {
	if r.upstream == nil {
		return nil, fmt.Errorf("kiro: HTTP upstream is not configured")
	}
	credentials, err := kiro.Parse(a.Credentials)
	if err != nil {
		return nil, err
	}
	return kiro.Refresh(ctx, kiroAccountClient{r.upstream, a}, credentials, a.Credentials)
}

// KiroTokenProvider always re-reads the account rather than trusting a stale
// scheduler snapshot. No access-token cache is populated, so credential edits
// cannot resurrect a previously cached token.
type KiroTokenProvider struct {
	repo        AccountRepository
	refreshAPI  *OAuthRefreshAPI
	refresher   *KiroTokenRefresher
	invalidator TokenCacheInvalidator
}

func (p *KiroTokenProvider) Account(ctx context.Context, a *Account, force bool) (*Account, error) {
	if a == nil || a.Platform != PlatformKiro {
		return nil, fmt.Errorf("kiro: invalid account")
	}
	rejectedToken := a.GetCredential("access_token")
	if p.repo != nil {
		fresh, err := p.repo.GetByID(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		if fresh == nil || fresh.Platform != PlatformKiro || !fresh.IsActive() {
			return nil, fmt.Errorf("kiro: account is unavailable")
		}
		a = fresh
	}
	if a.Type == AccountTypeAPIKey {
		return a, nil
	}
	if a.Type != AccountTypeOAuth || p.refreshAPI == nil {
		return nil, fmt.Errorf("kiro: OAuth provider is not configured")
	}
	executor := *p.refresher
	executor.force = force
	if force {
		executor.rejectedToken = rejectedToken
	}
	for attempt := 0; attempt < 25; attempt++ {
		result, err := p.refreshAPI.RefreshIfNeeded(withOAuthRefreshRequestPath(ctx), a, &executor, 3*time.Minute)
		if err != nil {
			return nil, err
		}
		if !result.LockHeld {
			if result.Account != nil {
				a = result.Account
			}
			if a.Platform != PlatformKiro || a.Type != AccountTypeOAuth || !a.IsActive() {
				return nil, fmt.Errorf("kiro: account changed during refresh")
			}
			if result.Refreshed && p.invalidator != nil {
				_ = p.invalidator.InvalidateToken(ctx, a)
			}
			if a.GetCredential("access_token") == "" {
				return nil, fmt.Errorf("kiro: missing access token after refresh")
			}
			return a, nil
		}
		if err = sleepWithContext(ctx, 200*time.Millisecond); err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("kiro: token refresh is busy; retry later")
}

func NormalizeKiroAccountCredentials(accountType string, raw map[string]any) (map[string]any, error) {
	normalized := kiro.Normalize(raw)
	if mapping, exists := normalized["model_mapping"]; exists && mapping != nil {
		valid := true
		switch values := mapping.(type) {
		case map[string]any:
			for key, value := range values {
				str, ok := value.(string)
				if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(str) == "" {
					valid = false
				}
			}
		case map[string]string:
			for key, value := range values {
				if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
					valid = false
				}
			}
		default:
			valid = false
		}
		if !valid {
			return nil, fmt.Errorf("kiro: model_mapping must map non-empty names to non-empty model IDs")
		}
	}
	credentials, err := kiro.Parse(normalized)
	if err != nil {
		return nil, err
	}
	if accountType != AccountTypeOAuth && accountType != AccountTypeAPIKey {
		return nil, fmt.Errorf("kiro: only OAuth and API Key account types are supported")
	}
	if (credentials.AuthMethod == "api_key") != (accountType == AccountTypeAPIKey) {
		return nil, fmt.Errorf("kiro: auth_method does not match account type")
	}
	return normalized, nil
}

func mergeKiroAccountCredentials(existing, incoming map[string]any) map[string]any {
	incoming = kiro.NormalizeAliases(incoming)
	merged := MergePreservingSensitiveCreds(existing, incoming)
	// Replacing a refresh identity must not keep using the previous identity's
	// still-valid access token. A supplied new access token remains authoritative.
	if _, supplied := incoming["access_token"]; !supplied {
		for _, key := range []string{"refresh_token", "auth_method", "auth_region", "client_id", "client_secret"} {
			if value, exists := incoming[key]; exists && !reflect.DeepEqual(value, existing[key]) {
				delete(merged, "access_token")
				delete(merged, "expires_at")
				break
			}
		}
	}
	return merged
}
