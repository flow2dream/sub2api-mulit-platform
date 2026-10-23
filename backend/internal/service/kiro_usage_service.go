package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	gocache "github.com/patrickmn/go-cache"
)

type kiroUsageCacheEntry struct {
	Quota *kiro.UsageLimits
	Err   error
}

func (s *KiroGatewayService) Usage(ctx context.Context, a *Account, force bool) (*kiro.UsageLimits, error) {
	a, err := s.tokens.Account(ctx, a, false)
	if err != nil {
		return nil, err
	}
	credentials, err := kiro.Parse(a.Credentials)
	if err != nil {
		return nil, err
	}
	identity, _ := json.Marshal(struct {
		Credentials map[string]any
		ProxyID     *int64
	}{a.Credentials, a.ProxyID})
	key := fmt.Sprintf("%d:%x", a.ID, sha256.Sum256(identity))
	if !force {
		if cached, ok := s.usageCache.Get(key); ok {
			entry := cached.(kiroUsageCacheEntry)
			return entry.Quota, entry.Err
		}
	}
	value, err, _ := s.usageFlight.Do(key, func() (any, error) {
		req, err := kiro.NewManagementRequest(ctx, credentials, "usage", "")
		if err != nil {
			return kiroUsageCacheEntry{Err: err}, nil
		}
		fetchCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		req = req.WithContext(fetchCtx)
		resp, err := (kiroAccountClient{s.upstream, a}).Do(req)
		entry := kiroUsageCacheEntry{}
		if err != nil {
			entry.Err = fmt.Errorf("kiro: usage query transport failed")
		} else {
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				entry.Err = &kiro.HTTPError{StatusCode: resp.StatusCode}
			} else {
				var quota kiro.UsageLimits
				if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&quota) != nil || len(quota.Breakdown) == 0 {
					entry.Err = fmt.Errorf("kiro: quota is unknown")
				} else {
					entry.Quota = &quota
				}
			}
		}
		ttl := time.Minute
		if entry.Err != nil {
			ttl = 30 * time.Second
		}
		s.usageCache.Set(key, entry, ttl)
		return entry, nil
	})
	if err != nil {
		return nil, err
	}
	entry := value.(kiroUsageCacheEntry)
	return entry.Quota, entry.Err
}

func (s *AccountUsageService) getKiroUsage(ctx context.Context, a *Account, force bool) (*UsageInfo, error) {
	now := time.Now()
	info := &UsageInfo{Source: "active", UpdatedAt: &now}
	if s.kiroGateway == nil {
		info.Error = "Kiro quota is unknown: gateway is not configured"
		return info, nil
	}
	quota, err := s.kiroGateway.Usage(ctx, a, force)
	if err != nil {
		info.Error = err.Error()
		info.ErrorCode = "quota_unknown"
		return info, nil
	}
	info.KiroQuota = quota
	return info, nil
}

// Keep cache creation at the provider boundary so zero-value test services do
// not accidentally introduce global state shared between accounts or servers.
func newKiroUsageCache() *gocache.Cache { return gocache.New(time.Minute, 5*time.Minute) }
