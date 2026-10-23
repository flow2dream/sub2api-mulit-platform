package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
)

type kiroProfileResult struct {
	ARN string
	Err error
}

var errKiroRuntimeProfile = errors.New("kiro: runtime OAuth profile discovery failed; import profile_arn from this account's Kiro credentials")

// Discovery is cached by credential identity and proxy, including failures, so
// repeated tests cannot hammer a profile operation unsupported by an account.
func (s *KiroGatewayService) inferenceCredentials(ctx context.Context, a *Account) (kiro.Credentials, error) {
	c, err := kiro.Parse(a.Credentials)
	if err != nil || c.EndpointFamily != "runtime" || c.AuthMethod == "api_key" || c.ProfileARN != "" {
		return c, err
	}
	identity, _ := json.Marshal(struct {
		Credentials map[string]any
		ProxyID     *int64
	}{a.Credentials, a.ProxyID})
	key := fmt.Sprintf("profile:%d:%x", a.ID, sha256.Sum256(identity))
	result, found := s.usageCache.Get(key)
	if !found {
		result, err, _ = s.usageFlight.Do(key, func() (any, error) {
			fetchCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			arn, fetchErr := kiro.DiscoverProfile(fetchCtx, kiroAccountClient{s.upstream, a}, c)
			entry := kiroProfileResult{arn, fetchErr}
			s.usageCache.Set(key, entry, time.Minute)
			return entry, nil
		})
		if err != nil {
			return c, err
		}
	}
	entry := result.(kiroProfileResult)
	if entry.Err != nil {
		return c, errKiroRuntimeProfile
	}
	c.ProfileARN = entry.ARN
	return c, nil
}
