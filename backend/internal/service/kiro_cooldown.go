package service

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type KiroCooldownRepository interface {
	SetKiroCooldownIfCredentialsUnchanged(context.Context, *Account, time.Time, string) (bool, error)
}

// Only explicit throttling/payment responses affect account availability. A
// forbidden model or regional policy must not disable unrelated model access.
func (s *KiroGatewayService) observeHTTPError(ctx context.Context, a *Account, status int, headers http.Header) time.Duration {
	delay := kiroCooldownDelay(status, headers, time.Now())
	if delay == 0 {
		return 0
	}
	repo, ok := s.tokens.repo.(KiroCooldownRepository)
	if !ok {
		return delay
	}
	reason := "kiro: upstream throttled"
	if status == http.StatusPaymentRequired {
		reason = "kiro: payment/quota unavailable; retry after cooldown"
	}
	stateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_, _ = repo.SetKiroCooldownIfCredentialsUnchanged(stateCtx, a, time.Now().Add(delay), reason)
	return delay
}

// The admin probe and gateway share the same bounded upstream retry policy.
func kiroCooldownDelay(status int, headers http.Header, now time.Time) time.Duration {
	if status != http.StatusTooManyRequests && status != http.StatusPaymentRequired {
		return 0
	}
	delay := time.Minute
	if status == http.StatusPaymentRequired {
		delay = 2 * time.Minute
	}
	if retry := strings.TrimSpace(headers.Get("Retry-After")); retry != "" {
		if seconds, err := strconv.ParseInt(retry, 10, 64); err == nil && seconds > 0 {
			delay = time.Duration(min(seconds, 24*60*60)) * time.Second
		} else if at, err := http.ParseTime(retry); err == nil && at.After(now) {
			delay = at.Sub(now)
		}
	}
	if delay > 24*time.Hour {
		delay = 24 * time.Hour
	}
	return delay
}
