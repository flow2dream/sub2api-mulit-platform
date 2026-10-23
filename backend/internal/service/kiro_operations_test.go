//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func kiroOAuthTestAccount() *Account {
	a := kiroTestAccount()
	a.Type = AccountTypeOAuth
	a.Credentials = map[string]any{"auth_method": "social", "access_token": "old-access", "refresh_token": "old-refresh", "expires_at": time.Now().Add(time.Hour).Unix()}
	return a
}

func kiroForwardForTest(s *KiroGatewayService, a *Account, ctx context.Context) (*ForwardResult, error, *httptest.ResponseRecorder) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}],"max_tokens":32}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	c.Set("api_key", &APIKey{ID: 11})
	result, err := s.Forward(ctx, c, a, body, APIProtocolAnthropic)
	return result, err, rec
}

func TestKiro401RefreshesAndRetriesOnlyOnce(t *testing.T) {
	for _, finalStatus := range []int{200, 401} {
		t.Run(http.StatusText(finalStatus), func(t *testing.T) {
			a := kiroOAuthTestAccount()
			repo := &kiroTestRepo{account: a}
			refreshes, inferences := 0, 0
			upstream := &kiroTestUpstream{call: func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Host, "auth.desktop") {
					refreshes++
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":3600}`))}, nil
				}
				inferences++
				if inferences == 1 {
					require.Equal(t, "Bearer old-access", r.Header.Get("Authorization"))
					return &http.Response{StatusCode: 401, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				require.Equal(t, "Bearer new-access", r.Header.Get("Authorization"))
				if finalStatus == 401 {
					return &http.Response{StatusCode: 401, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				return kiroNativeResponse([2]string{"assistantResponseEvent", `{"content":"OK"}`}, [2]string{"metadataEvent", `{"stopReason":"end_turn"}`}), nil
			}}
			s := ProvideKiroGatewayService(repo, upstream, NewOAuthRefreshAPI(repo, nil), nil, nil)
			_, err, _ := kiroForwardForTest(s, a, context.Background())
			if finalStatus == 200 {
				require.NoError(t, err)
			} else {
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				require.Equal(t, 401, failover.StatusCode)
			}
			require.Equal(t, 1, refreshes)
			require.Equal(t, 2, inferences)
		})
	}
}

func TestKiro401UsesConcurrentReplacementWithoutRefreshingAgain(t *testing.T) {
	a := kiroOAuthTestAccount()
	repo := &kiroTestRepo{account: a}
	calls := 0
	upstream := &kiroTestUpstream{call: func(r *http.Request) (*http.Response, error) {
		require.NotContains(t, r.URL.Host, "auth.desktop")
		calls++
		if calls == 1 {
			repo.mu.Lock()
			repo.account.Credentials = shallowCopyMap(a.Credentials)
			repo.account.Credentials["access_token"] = "replacement"
			repo.mu.Unlock()
			return &http.Response{StatusCode: 401, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		require.Equal(t, "Bearer replacement", r.Header.Get("Authorization"))
		return kiroNativeResponse([2]string{"assistantResponseEvent", `{"content":"OK"}`}, [2]string{"metadataEvent", `{"stopReason":"end_turn"}`}), nil
	}}
	s := ProvideKiroGatewayService(repo, upstream, NewOAuthRefreshAPI(repo, nil), nil, nil)
	_, err, _ := kiroForwardForTest(s, a, context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, calls)
}

type kiroCooldownTestRepo struct {
	*kiroTestRepo
	until time.Time
	calls int
}

func (r *kiroCooldownTestRepo) SetKiroCooldownIfCredentialsUnchanged(_ context.Context, _ *Account, until time.Time, _ string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.until = until
	r.calls++
	r.account.TempUnschedulableUntil = &until
	return true, nil
}

func kiroRunAccountProbe(s *AccountTestService, a *Account) (string, error) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/test", nil)
	err := s.testKiroAccountConnection(c, a, "claude-haiku-4.5", "hi", "default")
	return rec.Body.String(), err
}

func TestKiroAccountProbeThrottlingPersistsAndHonorsCooldown(t *testing.T) {
	a := kiroOAuthTestAccount()
	stale := *a
	repo := &kiroCooldownTestRepo{kiroTestRepo: &kiroTestRepo{account: a}}
	calls := 0
	upstream := &kiroTestUpstream{call: func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {"90"}}, Body: io.NopCloser(strings.NewReader(`{"message":"do not expose this secret"}`))}, nil
	}}
	gateway := ProvideKiroGatewayService(repo, upstream, NewOAuthRefreshAPI(repo, nil), nil, nil)
	tester := &AccountTestService{kiroGateway: gateway, httpUpstream: upstream}
	body, err := kiroRunAccountProbe(tester, &stale)
	require.Error(t, err)
	require.Contains(t, body, `"code":"kiro_throttled"`)
	require.Contains(t, body, `"retry_after_seconds":90`)
	require.NotContains(t, body, `"type":"test_start"`)
	require.NotContains(t, body, "do not expose this secret")
	require.Equal(t, 1, repo.calls)
	// Reopening a dialog with an old account snapshot must use current DB state.
	body, err = kiroRunAccountProbe(tester, &stale)
	require.Error(t, err)
	require.Contains(t, body, `"code":"kiro_cooldown"`)
	require.Equal(t, 1, calls)
}

func TestKiroAccountProbeRejectsConcurrentRequests(t *testing.T) {
	a := kiroOAuthTestAccount()
	repo := &kiroTestRepo{account: a}
	started, release := make(chan struct{}), make(chan struct{})
	upstream := &kiroTestUpstream{call: func(r *http.Request) (*http.Response, error) {
		close(started)
		<-release
		return kiroNativeResponse([2]string{"assistantResponseEvent", `{"content":"OK"}`}, [2]string{"metadataEvent", `{"stopReason":"end_turn"}`}), nil
	}}
	gateway := ProvideKiroGatewayService(repo, upstream, NewOAuthRefreshAPI(repo, nil), nil, nil)
	tester := &AccountTestService{kiroGateway: gateway, httpUpstream: upstream}
	type result struct {
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() { body, err := kiroRunAccountProbe(tester, a); done <- result{body, err} }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("probe did not start")
	}
	body, err := kiroRunAccountProbe(tester, a)
	close(release)
	first := <-done
	require.Error(t, err)
	require.Contains(t, body, `"code":"kiro_test_busy"`)
	require.NoError(t, first.err)
	require.Contains(t, first.body, `"type":"test_complete","success":true`)
}

func TestKiroRuntimeProfileDiscoveryCachesFailureAndKeepsCredentials(t *testing.T) {
	a := kiroOAuthTestAccount()
	a.Credentials["endpoint_family"] = "runtime"
	repo := &kiroTestRepo{account: a}
	calls := 0
	upstream := &kiroTestUpstream{call: func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"message":"secret-profile-response"}`))}, nil
	}}
	gateway := ProvideKiroGatewayService(repo, upstream, NewOAuthRefreshAPI(repo, nil), nil, nil)
	for i := 0; i < 2; i++ {
		_, err := gateway.inferenceCredentials(context.Background(), a)
		require.ErrorContains(t, err, "profile discovery failed")
		require.NotContains(t, err.Error(), "secret-profile-response")
	}
	require.Equal(t, 1, calls)
	require.NotContains(t, a.Credentials, "profile_arn")
}

func TestKiroRuntimeConcreteModelUsesDiscoveredProfile(t *testing.T) {
	a := kiroOAuthTestAccount()
	a.Credentials["endpoint_family"] = "runtime"
	repo := &kiroTestRepo{account: a}
	profiles, inferences := 0, 0
	upstream := &kiroTestUpstream{call: func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "codewhisperer.us-east-1.amazonaws.com" {
			profiles++
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"profiles":[{"arn":"arn:aws:codewhisperer:us-east-1:123456789012:profile/example"}]}`))}, nil
		}
		inferences++
		require.Equal(t, "https://runtime.us-east-1.kiro.dev/generateAssistantResponse", r.URL.String())
		body, _ := io.ReadAll(r.Body)
		require.Equal(t, "claude-sonnet-4.5", gjson.GetBytes(body, "conversationState.currentMessage.userInputMessage.modelId").String())
		require.Equal(t, "AI_EDITOR", gjson.GetBytes(body, "conversationState.currentMessage.userInputMessage.origin").String())
		require.Equal(t, "arn:aws:codewhisperer:us-east-1:123456789012:profile/example", gjson.GetBytes(body, "profileArn").String())
		return kiroNativeResponse([2]string{"assistantResponseEvent", `{"content":"OK"}`}, [2]string{"metadataEvent", `{"stopReason":"end_turn"}`}), nil
	}}
	gateway := ProvideKiroGatewayService(repo, upstream, NewOAuthRefreshAPI(repo, nil), nil, nil)
	tester := &AccountTestService{kiroGateway: gateway, httpUpstream: upstream}
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("POST", "/test", nil)
		require.NoError(t, tester.testKiroAccountConnection(c, a, "claude-sonnet-4.5", "hi", "default"))
	}
	require.Equal(t, 1, profiles)
	require.Equal(t, 2, inferences)
	require.NotContains(t, a.Credentials, "profile_arn")
}

func TestKiroAutoOnLegacyEndpointRejectedBeforeInference(t *testing.T) {
	a := kiroOAuthTestAccount()
	repo := &kiroTestRepo{account: a}
	upstream := &kiroTestUpstream{call: func(r *http.Request) (*http.Response, error) {
		t.Fatal("must not send unsupported auto upstream")
		return nil, nil
	}}
	gateway := ProvideKiroGatewayService(repo, upstream, NewOAuthRefreshAPI(repo, nil), nil, nil)
	tester := &AccountTestService{kiroGateway: gateway, httpUpstream: upstream}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/test", nil)
	require.Error(t, tester.testKiroAccountConnection(c, a, "auto", "hi", "default"))
	require.Contains(t, rec.Body.String(), `"code":"kiro_auto_endpoint"`)
}

func TestKiroRuntimeAutoRejectedWithoutProfileLookup(t *testing.T) {
	a := kiroOAuthTestAccount()
	a.Credentials["endpoint_family"] = "runtime"
	a.Credentials["model_mapping"] = map[string]any{"auto-alias": "auto"}
	repo := &kiroTestRepo{account: a}
	upstream := &kiroTestUpstream{call: func(r *http.Request) (*http.Response, error) {
		t.Fatal("Auto must not trigger profile discovery or inference")
		return nil, nil
	}}
	gateway := ProvideKiroGatewayService(repo, upstream, NewOAuthRefreshAPI(repo, nil), nil, nil)
	tester := &AccountTestService{kiroGateway: gateway, httpUpstream: upstream}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/test", nil)
	require.Error(t, tester.testKiroAccountConnection(c, a, "auto-alias", "hi", "default"))
	require.Contains(t, rec.Body.String(), `"code":"kiro_auto_endpoint"`)
	for _, protocol := range []string{APIProtocolAnthropic, APIProtocolChatCompletions, APIProtocolResponses} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("POST", "/test", nil)
		body := []byte(`{"model":"auto-alias","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`)
		if protocol == APIProtocolResponses {
			body = []byte(`{"model":"auto-alias","input":"hi"}`)
		}
		_, err := gateway.Forward(context.Background(), c, a, body, protocol)
		require.ErrorContains(t, err, "Auto is not supported")
		require.Equal(t, 400, rec.Code)
	}
}

func TestKiroAccountProbeDiscoversConcreteDefaultModel(t *testing.T) {
	a := kiroOAuthTestAccount()
	delete(a.Credentials, "model_mapping")
	repo := &kiroTestRepo{account: a}
	upstream := &kiroTestUpstream{call: func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"models":[{"modelId":"auto"},{"modelId":"claude-haiku-4.5"},{"modelId":"claude-sonnet-4.5"},{"modelId":"minimax-m2.5"}]}`))}, nil
		}
		body, _ := io.ReadAll(r.Body)
		require.Equal(t, "minimax-m2.5", gjson.GetBytes(body, "conversationState.currentMessage.userInputMessage.modelId").String())
		require.Equal(t, "hi", gjson.GetBytes(body, "conversationState.currentMessage.userInputMessage.content").String())
		return kiroNativeResponse([2]string{"assistantResponseEvent", `{"content":"Hello! How can I help?"}`}, [2]string{"contextUsageEvent", `{"contextUsagePercentage":1}`}, [2]string{"meteringEvent", `{"usage":0.02}`}), nil
	}}
	gateway := ProvideKiroGatewayService(repo, upstream, NewOAuthRefreshAPI(repo, nil), nil, nil)
	tester := &AccountTestService{kiroGateway: gateway, httpUpstream: upstream}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/test", nil)
	require.NoError(t, tester.testKiroAccountConnection(c, a, "", "", "default"))
	require.Contains(t, rec.Body.String(), "Hello! How can I help?")
	require.Contains(t, rec.Body.String(), `"type":"test_complete","success":true`)
}

func TestKiroCooldownDelayHandlesRetryAfter(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		status int
		retry  string
		want   time.Duration
	}{
		{429, "", time.Minute},
		{429, "invalid", time.Minute},
		{429, "-2", time.Minute},
		{429, "0", time.Minute},
		{429, "90", 90 * time.Second},
		{429, now.Add(75 * time.Second).Format(http.TimeFormat), 75 * time.Second},
		{429, "9223372036854775807", 24 * time.Hour},
		{402, "", 2 * time.Minute},
		{403, "90", 0},
	} {
		require.Equal(t, tc.want, kiroCooldownDelay(tc.status, http.Header{"Retry-After": {tc.retry}}, now))
	}
}

func TestKiroCooldownOnlyOnExplicitThrottleOrPayment(t *testing.T) {
	a := kiroTestAccount()
	repo := &kiroCooldownTestRepo{kiroTestRepo: &kiroTestRepo{account: a}}
	s := ProvideKiroGatewayService(repo, nil, nil, nil, nil)
	for _, status := range []int{401, 403, 500} {
		s.observeHTTPError(context.Background(), a, status, http.Header{})
	}
	require.Zero(t, repo.calls)
	before := time.Now()
	s.observeHTTPError(context.Background(), a, 429, http.Header{"Retry-After": {"90"}})
	require.Equal(t, 1, repo.calls)
	require.InDelta(t, 90, repo.until.Sub(before).Seconds(), 1)
	s.observeHTTPError(context.Background(), a, 429, http.Header{"Retry-After": {"999999"}})
	require.InDelta(t, 24*time.Hour.Seconds(), time.Until(repo.until).Seconds(), 1)
	s.observeHTTPError(context.Background(), a, 402, http.Header{})
	require.InDelta(t, 120, time.Until(repo.until).Seconds(), 1)
}

func TestKiroModelsPaginationAndRepeatedToken(t *testing.T) {
	for _, repeat := range []bool{false, true} {
		t.Run(map[bool]string{false: "pages", true: "loop"}[repeat], func(t *testing.T) {
			a := kiroOAuthTestAccount()
			repo := &kiroTestRepo{account: a}
			calls := 0
			upstream := &kiroTestUpstream{call: func(r *http.Request) (*http.Response, error) {
				calls++
				require.Equal(t, "codewhisperer.us-east-1.amazonaws.com", r.URL.Host)
				data := `{"models":[{"modelId":"first"}],"nextToken":"second"}`
				if calls == 2 {
					require.Equal(t, "second", r.URL.Query().Get("nextToken"))
					if !repeat {
						data = `{"models":[{"modelId":"second"}]}`
					}
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(data))}, nil
			}}
			s := ProvideKiroGatewayService(repo, upstream, NewOAuthRefreshAPI(repo, nil), nil, nil)
			tester := &AccountTestService{kiroGateway: s, httpUpstream: upstream}
			ids, _, err := tester.fetchKiroUpstreamModels(context.Background(), a)
			if repeat {
				require.ErrorContains(t, err, "repeated")
			} else {
				require.NoError(t, err)
				require.Equal(t, []string{"first", "second"}, ids)
			}
			require.Equal(t, 2, calls)
		})
	}
}

func TestKiroAccountModelsDiscoverNativeNamesAndProjectMappings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mapping map[string]any
		want    []string
	}{
		{"no mapping", nil, []string{"claude-sonnet-4.5"}},
		{"auto alias", map[string]any{"auto-alias": "auto", "sonnet": "claude-sonnet-4.5"}, []string{"sonnet"}},
		{"alias", map[string]any{"my-model": "claude-sonnet-4.5", "removed": "not-available"}, []string{"my-model"}},
		{"version alias", map[string]any{"claude-sonnet-4-5-20250929": "claude-sonnet-4-5-20250929"}, []string{"claude-sonnet-4-5-20250929"}},
		{"wildcard", map[string]any{"claude-*": "claude-sonnet-4.5"}, []string{"claude-sonnet-4.5"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := kiroOAuthTestAccount()
			if tc.mapping != nil {
				a.Credentials["model_mapping"] = tc.mapping
			}
			repo := &kiroTestRepo{account: a}
			upstream := &kiroTestUpstream{call: func(req *http.Request) (*http.Response, error) {
				require.Equal(t, "/ListAvailableModels", req.URL.Path)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"models":[{"modelId":"claude-sonnet-4.5","modelName":"Claude Sonnet 4.5"},{"modelId":"auto","modelName":"Auto"}]}`))}, nil
			}}
			gateway := ProvideKiroGatewayService(repo, upstream, NewOAuthRefreshAPI(repo, nil), nil, nil)
			tester := &AccountTestService{kiroGateway: gateway, httpUpstream: upstream}
			models, err := tester.FetchKiroAccountModels(context.Background(), a)
			require.NoError(t, err)
			ids := make([]string, 0, len(models))
			for _, model := range models {
				ids = append(ids, model.ID)
				require.NotEmpty(t, model.DisplayName)
			}
			require.Equal(t, tc.want, ids)
			if tc.name == "alias" {
				require.Equal(t, "my-model (Claude Sonnet 4.5)", models[0].DisplayName)
			}
			if tc.mapping == nil {
				require.NotContains(t, a.Credentials, "model_mapping")
			}
		})
	}
}

func TestKiroAccountModelsDoNotInventModelsOnUpstreamFailure(t *testing.T) {
	a := kiroOAuthTestAccount()
	repo := &kiroTestRepo{account: a}
	upstream := &kiroTestUpstream{call: func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(`{"error":"sensitive-upstream-body"}`))}, nil
	}}
	gateway := ProvideKiroGatewayService(repo, upstream, NewOAuthRefreshAPI(repo, nil), nil, nil)
	tester := &AccountTestService{kiroGateway: gateway, httpUpstream: upstream}
	models, err := tester.FetchKiroAccountModels(context.Background(), a)
	require.Nil(t, models)
	require.ErrorContains(t, err, "403")
	require.NotContains(t, err.Error(), "sensitive-upstream-body")
}

func TestKiroRuntimeModelCatalogExcludesAuto(t *testing.T) {
	a := kiroOAuthTestAccount()
	a.Credentials["endpoint_family"] = "runtime"
	delete(a.Credentials, "model_mapping")
	repo := &kiroTestRepo{account: a}
	upstream := &kiroTestUpstream{call: func(req *http.Request) (*http.Response, error) {
		require.Equal(t, "/ListAvailableModels", req.URL.Path)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"models":[{"modelId":"auto"},{"modelId":"claude-sonnet-4.5"}]}`))}, nil
	}}
	gateway := ProvideKiroGatewayService(repo, upstream, NewOAuthRefreshAPI(repo, nil), nil, nil)
	tester := &AccountTestService{kiroGateway: gateway, httpUpstream: upstream}
	models, err := tester.FetchKiroAccountModels(context.Background(), a)
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.Equal(t, "claude-sonnet-4.5", models[0].ID)
}

func TestKiroAccountModelsAPIKeyRequiresExplicitMappings(t *testing.T) {
	a := kiroTestAccount()
	a.Type = AccountTypeAPIKey
	a.Credentials = map[string]any{"api_key": "test-key"}
	tester := &AccountTestService{}
	_, err := tester.FetchKiroAccountModels(context.Background(), a)
	require.ErrorContains(t, err, "configure model mappings")
	a.Credentials["model_mapping"] = map[string]any{"my-model": "native-model"}
	models, err := tester.FetchKiroAccountModels(context.Background(), a)
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.Equal(t, "my-model", models[0].ID)
}

func TestKiroQuotaCacheAndUnknown(t *testing.T) {
	a := kiroOAuthTestAccount()
	repo := &kiroTestRepo{account: a}
	calls := 0
	upstream := &kiroTestUpstream{call: func(r *http.Request) (*http.Response, error) {
		calls++
		data := `{"usageBreakdownList":[{"resourceType":"CREDIT","usageLimit":100,"currentUsage":4}]}`
		if calls > 1 {
			data = `{}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(data))}, nil
	}}
	s := ProvideKiroGatewayService(repo, upstream, NewOAuthRefreshAPI(repo, nil), nil, nil)
	quota, err := s.Usage(context.Background(), a, false)
	require.NoError(t, err)
	require.Len(t, quota.Breakdown, 1)
	_, err = s.Usage(context.Background(), a, false)
	require.NoError(t, err)
	require.Equal(t, 1, calls)
	quota, err = s.Usage(context.Background(), a, true)
	require.ErrorContains(t, err, "unknown")
	require.Nil(t, quota)
	_, err = s.Usage(context.Background(), a, false)
	require.Error(t, err)
	require.Equal(t, 2, calls)
}

func TestKiroBridgePreservesSamplingAndLimitsWithoutVendorDefaults(t *testing.T) {
	s := &KiroGatewayService{}
	for _, protocol := range []string{APIProtocolResponses, APIProtocolChatCompletions} {
		body := `{"model":"claude-sonnet-5-5","input":"hi","max_output_tokens":32,"temperature":0.3}`
		if protocol == APIProtocolChatCompletions {
			body = `{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}],"max_tokens":32,"temperature":0.3}`
		}
		req, _, err := s.normalize([]byte(body), protocol)
		require.NoError(t, err)
		require.Equal(t, 32, req.MaxTokens)
		require.Equal(t, 0.3, *req.Temperature)
		require.Nil(t, req.Thinking)
		require.Nil(t, req.OutputConfig)
		_, err = kiro.BuildRequest(req, kiro.Credentials{AuthMethod: "api_key", EndpointFamily: "runtime"}, "test")
		require.NoError(t, err)
	}
	for _, body := range []string{
		`{"model":"m","input":"hi","tools":[{"type":"code_interpreter","container":{"type":"auto"}}]}`,
		`{"model":"m","input":"hi","reasoning":{"effort":"high"}}`,
		`{"model":"m","input":"hi","store":true}`,
	} {
		req, _, err := s.normalize([]byte(body), APIProtocolResponses)
		if err == nil {
			_, err = kiro.BuildRequest(req, kiro.Credentials{}, "test")
		}
		require.Error(t, err)
	}
	_, _, err := s.normalize([]byte(`{"model":"m","messages":[],"stop":"END"}`), APIProtocolChatCompletions)
	require.ErrorContains(t, err, "stop")
}

type kiroMeteringTestRepo struct {
	requestID string
	apiKeyID  int64
	usage     kiro.Usage
}

func (r *kiroMeteringTestRepo) RecordKiroUsage(_ context.Context, id string, _ int64, apiKeyID int64, _ string, usage kiro.Usage) error {
	r.requestID = id
	r.apiKeyID = apiKeyID
	r.usage = usage
	return nil
}

func TestKiroMeteringUsesBillingRequestIdentity(t *testing.T) {
	a := kiroTestAccount()
	repo := &kiroTestRepo{account: a}
	meter := &kiroMeteringTestRepo{}
	upstream := &kiroTestUpstream{call: func(*http.Request) (*http.Response, error) {
		return kiroNativeResponse([2]string{"assistantResponseEvent", `{"content":"OK"}`}, [2]string{"metadataEvent", `{"stopReason":"end_turn"}`}), nil
	}}
	s := ProvideKiroGatewayService(repo, upstream, nil, nil, nil)
	s.usageRepo = meter
	ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "stable-client-id")
	result, err, _ := kiroForwardForTest(s, a, ctx)
	require.NoError(t, err)
	require.Equal(t, resolveUsageBillingRequestID(ctx, result.RequestID), meter.requestID)
	require.Equal(t, int64(11), meter.apiKeyID)
	require.Equal(t, "estimated", meter.usage.Source)
}

func TestKiroResponsesCustomToolRestoresRawInput(t *testing.T) {
	a := kiroTestAccount()
	repo := &kiroTestRepo{account: a}
	for _, stream := range []bool{false, true} {
		upstream := &kiroTestUpstream{call: func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			require.Contains(t, string(body), `"name":"apply_patch"`)
			return kiroNativeResponse([2]string{"toolUseEvent", `{"toolUseId":"patch1","name":"apply_patch","input":"{\"input\":\"*** Begin Patch\\n*** End Patch\"}","stop":true}`}, [2]string{"metadataEvent", `{"stopReason":"tool_use"}`}), nil
		}}
		s := ProvideKiroGatewayService(repo, upstream, nil, nil, nil)
		body, _ := json.Marshal(map[string]any{"model": "m", "input": "edit", "stream": stream, "tools": []any{map[string]any{"type": "custom", "name": "apply_patch"}}})
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(body))
		_, err := s.Forward(context.Background(), c, a, body, APIProtocolResponses)
		require.NoError(t, err)
		response := rec.Body.String()
		if stream {
			for _, line := range strings.Split(response, "\n") {
				if strings.HasPrefix(line, "data: ") {
					raw := strings.TrimPrefix(line, "data: ")
					if gjson.Get(raw, "type").String() == "response.completed" {
						response = gjson.Get(raw, "response").Raw
					}
				}
			}
		}
		require.Equal(t, "custom_tool_call", gjson.Get(response, "output.0.type").String())
		require.Equal(t, "*** Begin Patch\n*** End Patch", gjson.Get(response, "output.0.input").String())
	}
}

func TestKiroHistoryRoundTripAndUnsupportedContent(t *testing.T) {
	s := &KiroGatewayService{}
	body := []byte(`{"model":"m","input":[{"role":"user","content":"read"},{"type":"reasoning","summary":[{"type":"summary_text","text":"inspect the file"}]},{"type":"function_call","call_id":"c1","name":"read","arguments":"{\"path\":\"a\"}"},{"type":"function_call_output","call_id":"c1","output":"contents"}],"tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]}`)
	req, _, err := s.normalize(body, APIProtocolResponses)
	require.NoError(t, err)
	payload, err := kiro.BuildRequest(req, kiro.Credentials{}, "test")
	require.NoError(t, err)
	require.Contains(t, payload.ConversationState.History[1].Assistant.Content, "inspect the file")
	require.Equal(t, "toolu_c1", payload.ConversationState.History[1].Assistant.ToolUses[0].ID)
	require.Equal(t, payload.ConversationState.History[1].Assistant.ToolUses[0].ID, payload.ConversationState.CurrentMessage.User.Context.ToolResults[0].ID)
	require.Equal(t, "contents", payload.ConversationState.CurrentMessage.User.Context.ToolResults[0].Content[0].Text)
	for _, input := range []string{
		`[{"role":"user","content":[{"type":"input_text","text":"hello"},{"type":"input_image","image_url":"https://example.com/a.png"}]}]`,
		`[{"role":"user","content":[{"type":"input_file","file_id":"f"}]}]`,
		`[{"role":"user","content":"hi"},{"type":"function_call","call_id":"missing","name":"f","arguments":"{}"}]`,
		`[{"role":"user","content":"hi"},{"type":"reasoning","encrypted_content":"ciphertext"}]`,
	} {
		_, _, err := s.normalize([]byte(`{"model":"m","input":`+input+`}`), APIProtocolResponses)
		require.Error(t, err)
	}
}

func TestKiroCredentialReplacementInvalidatesOldAccessToken(t *testing.T) {
	existing := map[string]any{"auth_method": "social", "refresh_token": "old", "access_token": "old-access", "expires_at": time.Now().Add(time.Hour).Unix()}
	merged := mergeKiroAccountCredentials(existing, map[string]any{"auth_method": "social", "refreshToken": "replacement"})
	require.Equal(t, "replacement", merged["refresh_token"])
	require.NotContains(t, merged, "access_token")
	require.NotContains(t, merged, "expires_at")
	require.Equal(t, "old-access", existing["access_token"])
	merged = mergeKiroAccountCredentials(existing, map[string]any{"auth_method": "social", "refreshToken": "replacement", "accessToken": "new-access"})
	require.Equal(t, "new-access", merged["access_token"])
	merged = mergeKiroAccountCredentials(existing, map[string]any{"auth_method": "social", "expires_at": existing["expires_at"]})
	require.Equal(t, "old-access", merged["access_token"])
	_, err := NormalizeKiroAccountCredentials(AccountTypeOAuth, map[string]any{"refresh_token": "r", "model_mapping": map[string]any{"m": 123}})
	require.ErrorContains(t, err, "model_mapping")
}

func TestKiroCompositeExplicitRouteKeepsManufacturerDefaults(t *testing.T) {
	resolver := NewCompositeRouteResolver(compositeRouteRepoStub{routes: []CompositeModelRoute{{ID: 1, GroupID: 7, PublicModel: "kiro-code", MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformKiro, UpstreamModel: "native-model", Endpoint: CompositeRouteEndpointAny, Enabled: true}}})
	for _, endpoint := range []string{CompositeRouteEndpointMessages, CompositeRouteEndpointChatCompletions, CompositeRouteEndpointResponses} {
		decision, err := resolver.Resolve(context.Background(), 7, "kiro-code", endpoint)
		require.NoError(t, err)
		require.True(t, decision.Matched)
		require.Equal(t, PlatformKiro, decision.TargetPlatform)
		require.Equal(t, "native-model", decision.UpstreamModel)
	}
	platform, ok := DetectModelPlatform("claude-sonnet-4")
	require.True(t, ok)
	require.Equal(t, PlatformAnthropic, platform)
	platform, ok = DetectModelPlatform("gpt-5.4")
	require.True(t, ok)
	require.Equal(t, PlatformOpenAI, platform)
}

func TestKiroCountTokensNeverCallsAnthropic(t *testing.T) {
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		a := kiroTestAccount()
		a.Type = accountType
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		parsed := &ParsedRequest{Model: "m", Body: NewRequestBodyRef([]byte(`{"model":"m","messages":[]}`))}
		require.NoError(t, (&GatewayService{}).ForwardCountTokens(context.Background(), c, a, parsed))
		require.Equal(t, http.StatusNotFound, rec.Code)
	}
}
