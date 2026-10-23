//go:build unit

package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type kiroTestRepo struct {
	AccountRepository
	mu        sync.Mutex
	account   *Account
	beforeCAS func()
	cooled    int
}

func (r *kiroTestRepo) GetByID(context.Context, int64) (*Account, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := *r.account
	a.Credentials = shallowCopyMap(a.Credentials)
	return &a, nil
}
func (r *kiroTestRepo) UpdateKiroOAuthCredentialsIfUnchanged(_ context.Context, _ int64, expected map[string]any, _ *int64, credentials map[string]any) (bool, error) {
	if r.beforeCAS != nil {
		r.beforeCAS()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !reflect.DeepEqual(expected, r.account.Credentials) {
		return false, nil
	}
	r.account.Credentials = shallowCopyMap(credentials)
	return true, nil
}
func (r *kiroTestRepo) SetTempUnschedulable(context.Context, int64, time.Time, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cooled++
	return nil
}
func (r *kiroTestRepo) SetRateLimited(context.Context, int64, time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cooled++
	return nil
}

type kiroTestUpstream struct {
	call func(*http.Request) (*http.Response, error)
}

func (u *kiroTestUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.call(req)
}
func (u *kiroTestUpstream) DoWithTLS(req *http.Request, proxy string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, n)
}
func kiroTestFrame(name, data string) []byte {
	var headers, frame bytes.Buffer
	for _, pair := range [][2]string{{":message-type", "event"}, {":event-type", name}} {
		headers.WriteByte(byte(len(pair[0])))
		headers.WriteString(pair[0])
		headers.WriteByte(7)
		_ = binary.Write(&headers, binary.BigEndian, uint16(len(pair[1])))
		headers.WriteString(pair[1])
	}
	_ = binary.Write(&frame, binary.BigEndian, uint32(16+headers.Len()+len(data)))
	_ = binary.Write(&frame, binary.BigEndian, uint32(headers.Len()))
	_ = binary.Write(&frame, binary.BigEndian, crc32.ChecksumIEEE(frame.Bytes()))
	frame.Write(headers.Bytes())
	frame.WriteString(data)
	_ = binary.Write(&frame, binary.BigEndian, crc32.ChecksumIEEE(frame.Bytes()))
	return frame.Bytes()
}
func kiroNativeResponse(parts ...[2]string) *http.Response {
	var data []byte
	for _, p := range parts {
		data = append(data, kiroTestFrame(p[0], p[1])...)
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"X-Amzn-Requestid": {"native-test"}}, Body: io.NopCloser(bytes.NewReader(data))}
}
func kiroTestAccount() *Account {
	return &Account{ID: 7, Platform: PlatformKiro, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"auth_method": "api_key", "api_key": "test-only", "endpoint_family": "runtime", "model_mapping": map[string]any{"public-model": "native-model"}}}
}

func TestKiroAllProtocolsStreamingAndBuffered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, protocol := range []string{APIProtocolAnthropic, APIProtocolChatCompletions, APIProtocolResponses} {
		for _, stream := range []bool{false, true} {
			t.Run(protocol+"/"+map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
				a := kiroTestAccount()
				repo := &kiroTestRepo{account: a}
				upstream := &kiroTestUpstream{call: func(r *http.Request) (*http.Response, error) {
					require.Equal(t, "runtime.us-east-1.kiro.dev", r.URL.Host)
					require.Equal(t, "API_KEY", r.Header.Get("tokentype"))
					require.Empty(t, r.Header.Get("anthropic-beta"))
					body, _ := io.ReadAll(r.Body)
					require.Contains(t, string(body), `"modelId":"native-model"`)
					require.NotContains(t, string(body), "You are Claude Code")
					return kiroNativeResponse(
						[2]string{"assistantResponseEvent", `{"content":"你好"}`},
						[2]string{"toolUseEvent", `{"toolUseId":"a","name":"read","input":"{\"path\":"}`},
						[2]string{"toolUseEvent", `{"toolUseId":"b","name":"read","input":"{\"path\":\"b\"}","stop":true}`},
						[2]string{"toolUseEvent", `{"toolUseId":"a","input":"\"a\"}","stop":true}`},
						[2]string{"meteringEvent", `{"usage":0.5}`},
						[2]string{"metadataEvent", `{"stopReason":"tool_use","usage":{"inputTokens":30,"outputTokens":12}}`}), nil
				}}
				s := ProvideKiroGatewayService(repo, upstream, NewOAuthRefreshAPI(repo, nil), nil, nil)
				body := map[string]any{"model": "public-model", "stream": stream, "max_tokens": 128}
				switch protocol {
				case APIProtocolAnthropic:
					body["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
					body["tools"] = []any{map[string]any{"name": "read", "input_schema": map[string]any{"type": "object"}}}
				case APIProtocolResponses:
					body["input"] = "hi"
					body["tools"] = []any{map[string]any{"type": "function", "name": "read", "parameters": map[string]any{"type": "object"}}}
				case APIProtocolChatCompletions:
					body["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
					body["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "read", "parameters": map[string]any{"type": "object"}}}}
				}
				data, _ := json.Marshal(body)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest("POST", "/", bytes.NewReader(data))
				result, err := s.Forward(context.Background(), c, a, data, protocol)
				require.NoError(t, err)
				require.Equal(t, 200, rec.Code)
				require.Equal(t, 30, result.Usage.InputTokens)
				require.Equal(t, 12, result.Usage.OutputTokens)
				require.Equal(t, "reported", result.UsageSource)
				require.Equal(t, 0.5, *result.UpstreamUsage.Credits)
				require.Equal(t, "native-model", result.UpstreamModel)
				text := rec.Body.String()
				require.Contains(t, text, "你好")
				require.Contains(t, text, "read")
				if stream {
					require.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")
					switch protocol {
					case APIProtocolAnthropic:
						require.Contains(t, text, "message_stop")
					case APIProtocolResponses:
						require.Contains(t, text, "response.completed")
					case APIProtocolChatCompletions:
						require.Contains(t, text, "[DONE]")
						require.Contains(t, text, `"finish_reason":"tool_calls"`)
					}
					// Both independent argument objects must survive the single-item
					// Responses encoder even though native tools were interleaved.
					arguments := kiroStreamArguments(protocol, text)
					require.JSONEq(t, `{"path":"a"}`, arguments["a"])
					require.JSONEq(t, `{"path":"b"}`, arguments["b"])
				} else {
					require.True(t, json.Valid(rec.Body.Bytes()))
					require.Equal(t, "reported", rec.Header().Get("X-Sub2API-Usage-Source"))
				}
			})
		}
	}
}

func TestKiroIDETrailersCompleteAllProtocols(t *testing.T) {
	for _, protocol := range []string{APIProtocolAnthropic, APIProtocolChatCompletions, APIProtocolResponses} {
		for _, stream := range []bool{false, true} {
			a := kiroOAuthTestAccount()
			delete(a.Credentials, "model_mapping")
			repo := &kiroTestRepo{account: a}
			upstream := &kiroTestUpstream{call: func(r *http.Request) (*http.Response, error) {
				return kiroNativeResponse([2]string{"assistantResponseEvent", `{"content":"OK"}`}, [2]string{"contextUsageEvent", `{"contextUsagePercentage":1}`}, [2]string{"meteringEvent", `{"usage":0.02}`}), nil
			}}
			gateway := ProvideKiroGatewayService(repo, upstream, NewOAuthRefreshAPI(repo, nil), nil, nil)
			body := map[string]any{"model": "minimax-m2.5", "max_tokens": 128, "stream": stream}
			if protocol == APIProtocolResponses {
				body["input"] = "hi"
			} else {
				body["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
			}
			data, _ := json.Marshal(body)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/", bytes.NewReader(data))
			result, err := gateway.Forward(context.Background(), c, a, data, protocol)
			require.NoError(t, err, "%s stream=%v", protocol, stream)
			require.True(t, result.UpstreamUsage.Complete)
			require.Contains(t, rec.Body.String(), "OK")
			if stream {
				end := map[string]string{APIProtocolAnthropic: "message_stop", APIProtocolChatCompletions: "[DONE]", APIProtocolResponses: "response.completed"}[protocol]
				require.Contains(t, rec.Body.String(), end)
			} else {
				require.True(t, json.Valid(rec.Body.Bytes()))
			}
		}
	}
}

func TestKiroTruncatedResponseNeverCompletesOrFailsOverAfterOutput(t *testing.T) {
	a := kiroTestAccount()
	repo := &kiroTestRepo{account: a}
	calls := 0
	upstream := &kiroTestUpstream{call: func(*http.Request) (*http.Response, error) {
		calls++
		return kiroNativeResponse([2]string{"assistantResponseEvent", `{"content":"partial"}`}, [2]string{"meteringEvent", `{"usage":0.125}`}), nil
	}}
	s := ProvideKiroGatewayService(repo, upstream, NewOAuthRefreshAPI(repo, nil), nil, nil)
	for _, protocol := range []string{APIProtocolAnthropic, APIProtocolResponses, APIProtocolChatCompletions} {
		body := []byte(`{"model":"public-model","stream":true,"messages":[{"role":"user","content":"hi"}],"input":"hi"}`)
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest("POST", "/", nil)
		result, err := s.Forward(context.Background(), c, a, body, protocol)
		require.Error(t, err)
		require.NotNil(t, result)
		var failover *UpstreamFailoverError
		require.NotErrorAs(t, err, &failover)
		require.NotContains(t, rec.Body.String(), "message_stop")
		require.NotContains(t, rec.Body.String(), "response.completed")
		require.NotContains(t, rec.Body.String(), "[DONE]")
		require.Contains(t, rec.Body.String(), "interrupted")
		require.Equal(t, 0.125, *result.UpstreamUsage.Credits)
		require.False(t, result.UpstreamUsage.Complete)
	}
	require.Equal(t, 3, calls)
}

func TestKiroConcurrentRefreshAndAdminReplacement(t *testing.T) {
	a := kiroTestAccount()
	a.Type = AccountTypeOAuth
	a.Credentials = map[string]any{"auth_method": "social", "refresh_token": "old", "expires_at": time.Now().Add(-time.Hour).Unix()}
	repo := &kiroTestRepo{account: a}
	var calls atomic.Int32
	upstream := &kiroTestUpstream{call: func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		require.Contains(t, r.URL.Host, "auth.desktop.kiro.dev")
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"accessToken":"new-access","refreshToken":"rotated","expiresIn":3600}`))}, nil
	}}
	s := ProvideKiroGatewayService(repo, upstream, NewOAuthRefreshAPI(repo, nil), nil, nil)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fresh, err := s.tokens.Account(context.Background(), a, false)
			if err != nil {
				t.Error(err)
				return
			}
			if fresh.GetCredential("refresh_token") != "rotated" {
				t.Error("lost rotation")
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), calls.Load())
	// A credential edit between upstream refresh and CAS must win.
	repo.beforeCAS = func() {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		repo.account.Credentials = map[string]any{"auth_method": "social", "refresh_token": "admin-replacement", "access_token": "admin-access", "expires_at": time.Now().Add(time.Hour).Unix()}
	}
	fresh, err := s.RefreshAccount(context.Background(), a)
	require.NoError(t, err)
	require.Equal(t, "admin-replacement", fresh.GetCredential("refresh_token"))
	require.Equal(t, "admin-access", fresh.GetCredential("access_token"))
}

func TestKiroRejectsStatefulResponsesAndRedactsOIDCSecrets(t *testing.T) {
	s := &KiroGatewayService{}
	_, _, err := s.normalize([]byte(`{"model":"m","input":"hi","previous_response_id":"resp_old"}`), APIProtocolResponses)
	require.ErrorContains(t, err, "complete history")
	for _, key := range []string{"client_secret", "clientSecret", "refreshToken", "accessToken", "apiKey"} {
		require.True(t, IsSensitiveCredentialKey(key))
	}
	credentials, err := NormalizeKiroAccountCredentials(AccountTypeOAuth, map[string]any{"authMethod": "IdC", "clientId": "c", "clientSecret": "s", "refreshToken": "r"})
	require.NoError(t, err)
	require.Equal(t, "idc", credentials["auth_method"])
}

// Reassemble the actual wire deltas as a client would; packet boundaries are
// deliberately different from JSON argument boundaries.
func kiroStreamArguments(protocol, wire string) map[string]string {
	result := map[string]string{}
	ids := map[int]string{}
	scanner := bufio.NewScanner(strings.NewReader(wire))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		raw := strings.TrimPrefix(line, "data: ")
		switch protocol {
		case APIProtocolAnthropic:
			index := int(gjson.Get(raw, "index").Int())
			if gjson.Get(raw, "content_block.type").String() == "tool_use" {
				ids[index] = gjson.Get(raw, "content_block.id").String()
			}
			if gjson.Get(raw, "delta.type").String() == "input_json_delta" {
				result[ids[index]] += gjson.Get(raw, "delta.partial_json").String()
			}
		case APIProtocolChatCompletions:
			for _, tool := range gjson.Get(raw, "choices.0.delta.tool_calls").Array() {
				index := int(tool.Get("index").Int())
				if id := tool.Get("id").String(); id != "" {
					ids[index] = id
				}
				result[ids[index]] += tool.Get("function.arguments").String()
			}
		case APIProtocolResponses:
			if gjson.Get(raw, "type").String() == "response.completed" {
				for _, item := range gjson.Get(raw, "response.output").Array() {
					if item.Get("type").String() == "function_call" {
						result[item.Get("call_id").String()] = item.Get("arguments").String()
					}
				}
			}
		}
	}
	return result
}
