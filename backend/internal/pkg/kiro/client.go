package kiro

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// HTTPError deliberately excludes upstream bodies which may contain credentials.
type HTTPError struct{ StatusCode int }

func (e *HTTPError) Error() string { return fmt.Sprintf("kiro upstream HTTP %d", e.StatusCode) }

func Authorize(req *http.Request, c Credentials) {
	req.Header.Set("Authorization", "Bearer "+c.Token())
	req.Header.Set("User-Agent", "sub2api-kiro/1.0")
	if c.AuthMethod == "api_key" {
		req.Header.Set("tokentype", "API_KEY")
	}
}

func NewInferenceRequest(ctx context.Context, c Credentials, payload *Payload) (*http.Request, error) {
	if c.EndpointFamily == "runtime" && c.AuthMethod != "api_key" && c.ProfileARN == "" {
		return nil, fmt.Errorf("kiro: runtime OAuth requires profile_arn; import the profile from this account's Kiro credentials")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint(), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	Authorize(req, c)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.amazon.eventstream")
	if c.EndpointFamily == "q" && c.AuthMethod != "api_key" {
		// IDE wire conventions verified against hank9999/kiro.rs (MIT).
		// Keep raw credentials out of client-identification headers.
		machine := fmt.Sprintf("%x", sha256.Sum256([]byte("KotlinNativeAPI/"+c.RefreshToken)))
		identity := "KiroIDE-0.11.107-" + machine
		req.Header.Set("X-Amz-User-Agent", "aws-sdk-js/1.0.34 "+identity)
		req.Header.Set("User-Agent", "aws-sdk-js/1.0.34 ua/2.1 os/linux lang/js md/nodejs#22.22.0 api/codewhispererstreaming#1.0.34 m/E "+identity)
		req.Header.Set("X-Amzn-Codewhisperer-Optout", "true")
		req.Header.Set("X-Amzn-Kiro-Agent-Mode", "vibe")
		req.Header.Set("Amz-Sdk-Invocation-Id", uuid.NewString())
		req.Header.Set("Amz-Sdk-Request", "attempt=1; max=1")
	}
	if c.EndpointFamily == "codewhisperer" || c.AuthMethod == "api_key" {
		req.Header.Set("X-Amz-Target", "AmazonCodeWhispererStreamingService.GenerateAssistantResponse")
	}
	if c.AuthMethod == "api_key" {
		req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	}
	return req, nil
}

func Refresh(ctx context.Context, client Doer, c Credentials, existing map[string]any) (map[string]any, error) {
	endpoint := "https://prod." + c.AuthRegion + ".auth.desktop.kiro.dev/refreshToken"
	body := map[string]string{"refreshToken": c.RefreshToken}
	if c.AuthMethod == "idc" {
		endpoint = "https://oidc." + c.AuthRegion + ".amazonaws.com/token"
		body["clientId"], body["clientSecret"], body["grantType"] = c.ClientID, c.ClientSecret, "refresh_token"
	} else if c.AuthMethod != "social" {
		return nil, fmt.Errorf("kiro: credentials do not support OAuth refresh")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kiro refresh transport failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{resp.StatusCode}
	}
	var result struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		ProfileARN   string `json:"profileArn"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("kiro: invalid refresh response")
	}
	if result.AccessToken == "" || result.ExpiresIn <= 0 || result.ExpiresIn > 365*24*3600 {
		return nil, fmt.Errorf("kiro: refresh response lacks valid access token/expiry")
	}
	out := Normalize(existing)
	out["access_token"], out["expires_at"] = result.AccessToken, time.Now().Add(time.Duration(result.ExpiresIn)*time.Second).Unix()
	if result.RefreshToken != "" {
		out["refresh_token"] = result.RefreshToken
	}
	if result.ProfileARN != "" {
		out["profile_arn"] = result.ProfileARN
	}
	return out, nil
}
