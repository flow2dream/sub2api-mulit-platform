package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/google/uuid"
)

type KiroOAuthSession struct {
	Login      kiro.DeviceLogin
	AuthRegion string
	ProxyURL   string
	NextPollAt int64
}

type KiroOAuthSessionStore interface {
	Save(context.Context, string, *KiroOAuthSession) error
	Load(context.Context, string) (*KiroOAuthSession, error)
	Delete(context.Context, string) error
	Lock(context.Context, string, string) (bool, error)
	Unlock(context.Context, string, string) error
}

type KiroOAuthService struct {
	proxyRepo ProxyRepository
	upstream  HTTPUpstream
	store     KiroOAuthSessionStore
}

func NewKiroOAuthService(proxyRepo ProxyRepository, upstream HTTPUpstream, store KiroOAuthSessionStore) *KiroOAuthService {
	return &KiroOAuthService{proxyRepo: proxyRepo, upstream: upstream, store: store}
}

type KiroOAuthResult struct {
	Status      string         `json:"status"`
	SessionID   string         `json:"session_id,omitempty"`
	AuthURL     string         `json:"auth_url,omitempty"`
	UserCode    string         `json:"user_code,omitempty"`
	ExpiresAt   int64          `json:"expires_at,omitempty"`
	RetryAfter  int            `json:"retry_after,omitempty"`
	Credentials map[string]any `json:"credentials,omitempty"`
}

type kiroOAuthClient struct {
	upstream HTTPUpstream
	proxy    string
}

func (c kiroOAuthClient) Do(req *http.Request) (*http.Response, error) {
	return c.upstream.Do(req, c.proxy, 0, 1)
}

func kiroSessionKey(owner int64, id string) (string, error) {
	if owner <= 0 {
		return "", infraerrors.Unauthorized("KIRO_OAUTH_AUTH_REQUIRED", "administrator login required")
	}
	if _, err := uuid.Parse(id); err != nil {
		return "", infraerrors.BadRequest("KIRO_OAUTH_SESSION_INVALID", "authorization session is invalid or expired")
	}
	return fmt.Sprintf("%d:%s", owner, id), nil
}

func (s *KiroOAuthService) Start(ctx context.Context, owner int64, region, startURL string, proxyID *int64) (*KiroOAuthResult, error) {
	id := uuid.NewString()
	key, err := kiroSessionKey(owner, id)
	if err != nil {
		return nil, err
	}
	region = strings.TrimSpace(region)
	if region == "" {
		region = "us-east-1"
	}
	// Validate before looking up the proxy or contacting AWS.
	if _, err = kiro.Parse(map[string]any{"auth_method": "idc", "auth_region": region, "refresh_token": "validation", "client_id": "validation", "client_secret": "validation"}); err != nil {
		return nil, infraerrors.BadRequest("KIRO_OAUTH_INVALID_REGION", "invalid authentication region")
	}
	if _, err = kiro.ValidateDeviceStartURL(startURL); err != nil {
		return nil, infraerrors.BadRequest("KIRO_OAUTH_INVALID_PORTAL", err.Error())
	}
	proxyURL := ""
	if proxyID != nil {
		proxy, err := s.proxyRepo.GetByID(ctx, *proxyID)
		if err != nil {
			return nil, err
		}
		if proxy == nil {
			return nil, infraerrors.BadRequest("KIRO_OAUTH_PROXY_NOT_FOUND", "proxy not found")
		}
		proxyURL = proxy.URL()
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	login, err := kiro.StartDeviceLogin(ctx, kiroOAuthClient{s.upstream, proxyURL}, region, startURL)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "KIRO_OAUTH_START_FAILED", err.Error())
	}
	session := &KiroOAuthSession{Login: *login, AuthRegion: region, ProxyURL: proxyURL, NextPollAt: time.Now().Unix() + int64(login.Interval)}
	if err = s.store.Save(ctx, key, session); err != nil {
		return nil, fmt.Errorf("kiro: could not store authorization session")
	}
	return &KiroOAuthResult{Status: "pending", SessionID: id, AuthURL: login.AuthURL, UserCode: login.UserCode, ExpiresAt: login.ExpiresAt, RetryAfter: login.Interval}, nil
}

func (s *KiroOAuthService) Poll(ctx context.Context, owner int64, id string) (*KiroOAuthResult, error) {
	key, err := kiroSessionKey(owner, id)
	if err != nil {
		return nil, err
	}
	lockID := uuid.NewString()
	locked, err := s.store.Lock(ctx, key, lockID)
	if err != nil {
		return nil, fmt.Errorf("kiro: authorization session store unavailable")
	}
	if !locked {
		return &KiroOAuthResult{Status: "pending", RetryAfter: 5}, nil
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.store.Unlock(releaseCtx, key, lockID)
	}()
	session, err := s.store.Load(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("kiro: could not read authorization session")
	}
	if session == nil {
		return nil, infraerrors.BadRequest("KIRO_OAUTH_SESSION_INVALID", "authorization session is invalid or expired")
	}
	now := time.Now().Unix()
	if now >= session.Login.ExpiresAt {
		_ = s.store.Delete(ctx, key)
		return &KiroOAuthResult{Status: "expired"}, nil
	}
	if now < session.NextPollAt {
		return &KiroOAuthResult{Status: "pending", RetryAfter: int(session.NextPollAt - now)}, nil
	}
	// Save throttling before contacting AWS, including on transport errors.
	session.NextPollAt = now + int64(session.Login.Interval)
	if err = s.store.Save(ctx, key, session); err != nil {
		return nil, fmt.Errorf("kiro: could not update authorization session")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	token, status, err := kiro.PollDeviceLogin(ctx, kiroOAuthClient{s.upstream, session.ProxyURL}, session.AuthRegion, &session.Login)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "KIRO_OAUTH_POLL_FAILED", err.Error())
	}
	result := &KiroOAuthResult{Status: status, RetryAfter: session.Login.Interval}
	switch status {
	case "slow_down":
		session.Login.Interval = min(60, session.Login.Interval+5)
		session.NextPollAt = time.Now().Unix() + int64(session.Login.Interval)
		result.Status, result.RetryAfter = "pending", session.Login.Interval
		if err = s.store.Save(ctx, key, session); err != nil {
			return nil, fmt.Errorf("kiro: could not update authorization session")
		}
	case "denied", "expired":
		if err = s.store.Delete(ctx, key); err != nil {
			return nil, fmt.Errorf("kiro: could not close authorization session")
		}
	case "completed":
		// Delete before returning credentials: a session can deliver them once.
		if err = s.store.Delete(ctx, key); err != nil {
			return nil, fmt.Errorf("kiro: could not consume authorization session")
		}
		result.RetryAfter = 0
		result.Credentials = map[string]any{"auth_method": "idc", "auth_region": session.AuthRegion, "access_token": token.AccessToken, "refresh_token": token.RefreshToken, "expires_at": time.Now().Unix() + token.ExpiresIn, "client_id": session.Login.ClientID, "client_secret": session.Login.ClientSecret, "client_secret_expires_at": session.Login.ClientSecretExpiresAt}
		if token.ProfileARN != "" {
			result.Credentials["profile_arn"] = token.ProfileARN
		}
	}
	return result, nil
}
