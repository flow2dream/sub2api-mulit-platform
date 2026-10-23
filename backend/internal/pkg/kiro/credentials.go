// Package kiro implements Kiro's native protocol independently of the gateway.
package kiro

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

type Credentials struct {
	AuthMethod, AccessToken, RefreshToken, APIKey string
	ClientID, ClientSecret, AuthRegion, APIRegion string
	ProfileARN, EndpointFamily                    string
}

var regionPattern = regexp.MustCompile(`^[a-z]{2}(?:-[a-z]+)+-\d+$`)

// Normalize accepts exported CLI/IDE camelCase fields without retaining aliases.
// The caller must merge redacted edit fields with stored credentials first.
func NormalizeAliases(raw map[string]any) map[string]any {
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		out[k] = v
	}
	for old, key := range map[string]string{"accessToken": "access_token", "refreshToken": "refresh_token", "clientId": "client_id", "clientSecret": "client_secret", "profileArn": "profile_arn", "expiresAt": "expires_at", "authMethod": "auth_method", "authRegion": "auth_region", "apiRegion": "api_region", "apiKey": "api_key", "endpointFamily": "endpoint_family"} {
		if _, ok := out[key]; !ok {
			if v, exists := out[old]; exists {
				out[key] = v
			}
		}
		delete(out, old)
	}
	if method, ok := out["auth_method"].(string); ok {
		out["auth_method"] = strings.ToLower(strings.TrimSpace(method))
		if out["auth_method"] == "builder-id" || out["auth_method"] == "iam" {
			out["auth_method"] = "idc"
		}
	}
	// kiro.rs names the Amazon Q endpoint "ide" in exported credentials.
	if _, explicit := out["endpoint_family"]; !explicit && out["endpoint"] == "ide" {
		out["endpoint_family"] = "q"
	}
	return out
}

func Normalize(raw map[string]any) map[string]any {
	out := NormalizeAliases(raw)
	str := func(k string) string { v, _ := out[k].(string); return strings.TrimSpace(v) }
	if str("auth_method") == "" {
		switch {
		case str("api_key") != "":
			out["auth_method"] = "api_key"
		case str("client_id") != "":
			out["auth_method"] = "idc"
		default:
			out["auth_method"] = "social"
		}
	}
	if str("auth_region") == "" {
		out["auth_region"] = "us-east-1"
		if str("region") != "" {
			out["auth_region"] = str("region")
		}
	}
	if str("api_region") == "" {
		out["api_region"] = "us-east-1"
		parts := strings.Split(str("profile_arn"), ":")
		if len(parts) > 3 && parts[3] != "" {
			out["api_region"] = parts[3]
		}
	}
	if str("endpoint_family") == "" {
		out["endpoint_family"] = "q"
		if str("auth_method") == "api_key" {
			out["endpoint_family"] = "runtime"
		}
	}
	if v := str("expires_at"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			out["expires_at"] = t.Unix()
		}
	}
	return out
}

func Parse(raw map[string]any) (Credentials, error) {
	n := Normalize(raw)
	s := func(k string) string { v, _ := n[k].(string); return strings.TrimSpace(v) }
	c := Credentials{AuthMethod: s("auth_method"), AccessToken: s("access_token"), RefreshToken: s("refresh_token"), APIKey: s("api_key"), ClientID: s("client_id"), ClientSecret: s("client_secret"), AuthRegion: s("auth_region"), APIRegion: s("api_region"), ProfileARN: s("profile_arn"), EndpointFamily: s("endpoint_family")}
	if !regionPattern.MatchString(c.AuthRegion) || !regionPattern.MatchString(c.APIRegion) {
		return c, fmt.Errorf("kiro: invalid auth_region or api_region")
	}
	switch c.AuthMethod {
	case "social":
		if c.RefreshToken == "" {
			return c, fmt.Errorf("kiro: social credentials require refresh_token")
		}
	case "idc":
		if c.RefreshToken == "" || c.ClientID == "" || c.ClientSecret == "" {
			return c, fmt.Errorf("kiro: OIDC requires refresh_token, client_id and client_secret")
		}
	case "api_key":
		if c.APIKey == "" || c.EndpointFamily != "runtime" {
			return c, fmt.Errorf("kiro: API Key requires api_key and runtime endpoint")
		}
	default:
		return c, fmt.Errorf("kiro: unsupported auth_method")
	}
	if c.EndpointFamily != "q" && c.EndpointFamily != "codewhisperer" && c.EndpointFamily != "runtime" {
		return c, fmt.Errorf("kiro: unsupported endpoint_family")
	}
	if c.ProfileARN != "" {
		p := strings.Split(c.ProfileARN, ":")
		if len(p) < 6 || p[0] != "arn" || p[3] != c.APIRegion {
			return c, fmt.Errorf("kiro: profile_arn region differs from api_region")
		}
	}
	return c, nil
}

func (c Credentials) Token() string {
	if c.AuthMethod == "api_key" {
		return c.APIKey
	}
	return c.AccessToken
}

func (c Credentials) Endpoint() string {
	switch c.EndpointFamily {
	case "runtime":
		if c.AuthMethod != "api_key" {
			return "https://runtime." + c.APIRegion + ".kiro.dev/generateAssistantResponse"
		}
		return "https://runtime." + c.APIRegion + ".kiro.dev/"
	case "codewhisperer":
		return "https://codewhisperer." + c.APIRegion + ".amazonaws.com/generateAssistantResponse"
	default:
		return "https://q." + c.APIRegion + ".amazonaws.com/generateAssistantResponse"
	}
}
