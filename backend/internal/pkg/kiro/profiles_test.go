package kiro

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

type profileDoer func(*http.Request) (*http.Response, error)

func (f profileDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestRuntimeOAuthAndAPIKeyUseDistinctProtocols(t *testing.T) {
	for _, method := range []string{"social", "idc", "api_key"} {
		t.Run(method, func(t *testing.T) {
			c := Credentials{AuthMethod: method, EndpointFamily: "runtime", APIRegion: "us-east-1", AccessToken: "access", APIKey: "key", ProfileARN: "arn:aws:codewhisperer:us-east-1:123456789012:profile/test"}
			payload, err := BuildRequest(&apicompat.AnthropicRequest{Model: "auto", Messages: []apicompat.AnthropicMessage{{Role: "user", Content: []byte(`"hi"`)}}}, c, "test")
			require.NoError(t, err)
			req, err := NewInferenceRequest(context.Background(), c, payload)
			require.NoError(t, err)
			if method == "api_key" {
				require.Equal(t, "/", req.URL.Path)
				require.Equal(t, "API_KEY", req.Header.Get("tokentype"))
				require.Equal(t, "KIRO_CLI", payload.ConversationState.CurrentMessage.User.Origin)
				require.Empty(t, payload.ProfileARN)
			} else {
				require.Equal(t, "/generateAssistantResponse", req.URL.Path)
				require.Equal(t, "application/json", req.Header.Get("Content-Type"))
				require.Empty(t, req.Header.Get("X-Amz-Target"))
				require.Equal(t, "AI_EDITOR", payload.ConversationState.CurrentMessage.User.Origin)
				require.Equal(t, c.ProfileARN, payload.ProfileARN)
				c.ProfileARN = ""
				_, err = NewInferenceRequest(context.Background(), c, payload)
				require.ErrorContains(t, err, "profile_arn")
			}
		})
	}
}

func TestProfileDiscoveryValidatesPaginationAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{
		{"one", `{"profiles":[{"arn":"arn:aws:codewhisperer:us-east-1:123456789012:profile/one"}]}`, true},
		{"empty", `{"profiles":[]}`, false},
		{"wrong region", `{"profiles":[{"arn":"arn:aws:codewhisperer:eu-west-1:123456789012:profile/one"}]}`, false},
		{"ambiguous", `{"profiles":[{"arn":"arn:aws:codewhisperer:us-east-1:123456789012:profile/one"},{"arn":"arn:aws:codewhisperer:us-east-1:123456789012:profile/two"}]}`, false},
		{"loop", `{"profiles":[],"nextToken":"same"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := profileDoer(func(r *http.Request) (*http.Response, error) {
				calls++
				require.Equal(t, "https://codewhisperer.us-east-1.amazonaws.com/", r.URL.String())
				require.Equal(t, "AmazonCodeWhispererService.ListAvailableProfiles", r.Header.Get("X-Amz-Target"))
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			arn, err := DiscoverProfile(context.Background(), client, Credentials{AuthMethod: "idc", APIRegion: "us-east-1"})
			if tc.ok {
				require.NoError(t, err)
				require.NotEmpty(t, arn)
			} else {
				require.Error(t, err)
				require.Empty(t, arn)
			}
			require.LessOrEqual(t, calls, 2)
		})
	}
}
