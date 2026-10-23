package kiro

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestIDERequestCompatibility(t *testing.T) {
	c := Credentials{AuthMethod: "idc", EndpointFamily: "q", APIRegion: "us-east-1", RefreshToken: "private-refresh-value", AccessToken: "private-access-value", ProfileARN: "arn:aws:codewhisperer:us-east-1:123456789012:profile/example"}
	input := &apicompat.AnthropicRequest{Model: "claude-sonnet-4-5-20250929", MaxTokens: 128, Messages: []apicompat.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hello"`)}}}
	p, err := BuildRequest(input, c, "conversation")
	require.NoError(t, err)
	require.Equal(t, "claude-sonnet-4-5-20250929", input.Model)
	require.Equal(t, "claude-sonnet-4.5", p.ConversationState.CurrentMessage.User.ModelID)
	require.Equal(t, "vibe", p.ConversationState.AgentTaskType)
	_, err = uuid.Parse(p.ConversationState.AgentContinuationID)
	require.NoError(t, err)
	req, err := NewInferenceRequest(context.Background(), c, p)
	require.NoError(t, err)
	require.Equal(t, "https://q.us-east-1.amazonaws.com/generateAssistantResponse", req.URL.String())
	require.Equal(t, "vibe", req.Header.Get("X-Amzn-Kiro-Agent-Mode"))
	require.Equal(t, "true", req.Header.Get("X-Amzn-Codewhisperer-Optout"))
	require.Equal(t, "attempt=1; max=1", req.Header.Get("Amz-Sdk-Request"))
	require.Empty(t, req.Header.Get("X-Amz-Target"))
	_, err = uuid.Parse(req.Header.Get("Amz-Sdk-Invocation-Id"))
	require.NoError(t, err)
	for _, header := range []string{"User-Agent", "X-Amz-User-Agent"} {
		require.Contains(t, req.Header.Get(header), "KiroIDE-")
		require.NotContains(t, req.Header.Get(header), c.RefreshToken)
		require.NotContains(t, req.Header.Get(header), c.AccessToken)
	}
	second, err := NewInferenceRequest(context.Background(), c, p)
	require.NoError(t, err)
	require.Equal(t, req.Header.Get("X-Amz-User-Agent"), second.Header.Get("X-Amz-User-Agent"))
	require.NotEqual(t, req.Header.Get("Amz-Sdk-Invocation-Id"), second.Header.Get("Amz-Sdk-Invocation-Id"))
	body, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), `"profileArn":"`+c.ProfileARN+`"`)
	require.Contains(t, string(body), `"origin":"AI_EDITOR"`)
	require.Contains(t, string(body), `"userInputMessageContext":{}`)
}

func TestIDEAliasesPreserveModelVersion(t *testing.T) {
	for input, want := range map[string]string{
		"claude-sonnet-4-5-20250929": "claude-sonnet-4.5",
		"claude-haiku-4-5":           "claude-haiku-4.5",
		"claude-opus-4.6":            "claude-opus-4.6",
		"claude-sonnet-4-20250514":   "claude-sonnet-4-20250514",
		"claude-sonnet-5":            "claude-sonnet-5",
		"deepseek-3.2":               "deepseek-3.2", "auto": "auto",
		"custom-claude-sonnet-4-5": "custom-claude-sonnet-4-5",
	} {
		require.Equal(t, want, NormalizeModelID(input), input)
	}
}

func TestIDEExportCredentials(t *testing.T) {
	for _, method := range []string{"builder-id", "IAM"} {
		c, err := Parse(map[string]any{"authMethod": method, "endpoint": "ide", "clientId": "client", "clientSecret": "secret", "refreshToken": "refresh", "region": "eu-west-1"})
		require.NoError(t, err)
		require.Equal(t, "idc", c.AuthMethod)
		require.Equal(t, "q", c.EndpointFamily)
		require.Equal(t, "eu-west-1", c.AuthRegion)
	}
	n := NormalizeAliases(map[string]any{"endpoint": "ide", "endpointFamily": "runtime"})
	require.Equal(t, "runtime", n["endpoint_family"])
}
