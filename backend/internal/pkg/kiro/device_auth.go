package kiro

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const BuilderIDStartURL = "https://view.awsapps.com/start"

type DeviceLogin struct {
	ClientID              string
	ClientSecret          string
	ClientSecretExpiresAt int64
	DeviceCode            string
	UserCode              string
	AuthURL               string
	Interval              int
	ExpiresAt             int64
}

type DeviceToken struct {
	ProfileARN   string `json:"profileArn"`
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
}

func ValidateDeviceStartURL(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return BuilderIDStartURL, nil
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || !strings.HasSuffix(u.Hostname(), ".awsapps.com") || (u.Path != "/start" && u.Path != "/start/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("kiro: enter an HTTPS AWS access portal URL ending in /start")
	}
	return u.String(), nil
}

func validDeviceAuthURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.User == nil && u.Port() == "" && (strings.HasSuffix(u.Hostname(), ".awsapps.com") || strings.HasSuffix(u.Hostname(), ".amazonaws.com"))
}

// deviceJSON never returns raw error bodies or device/client secrets.
func deviceJSON(ctx context.Context, client Doer, region, path string, payload any, result any) (string, error) {
	if !regionPattern.MatchString(region) {
		return "", fmt.Errorf("kiro: invalid authentication region")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oidc."+region+".amazonaws.com"+path, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("kiro: device authorization connection failed")
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return "", fmt.Errorf("kiro: invalid device authorization response")
	}
	if resp.StatusCode != http.StatusOK {
		var failure struct {
			Error string `json:"error"`
			Type  string `json:"__type"`
		}
		_ = json.Unmarshal(data, &failure)
		code := failure.Error
		if code == "" {
			code = failure.Type
		}
		if resp.StatusCode == http.StatusBadRequest && path == "/token" {
			switch code {
			case "authorization_pending", "AuthorizationPendingException":
				return "pending", nil
			case "slow_down", "SlowDownException":
				return "slow_down", nil
			case "expired_token", "ExpiredTokenException":
				return "expired", nil
			case "access_denied", "AccessDeniedException":
				return "denied", nil
			}
		}
		return "", &HTTPError{StatusCode: resp.StatusCode}
	}
	if json.Unmarshal(data, result) != nil {
		return "", fmt.Errorf("kiro: invalid device authorization JSON")
	}
	return "completed", nil
}

func StartDeviceLogin(ctx context.Context, client Doer, region, startURL string) (*DeviceLogin, error) {
	portal, err := ValidateDeviceStartURL(startURL)
	if err != nil {
		return nil, err
	}
	var registered struct {
		ClientID              string `json:"clientId"`
		ClientSecret          string `json:"clientSecret"`
		ClientSecretExpiresAt int64  `json:"clientSecretExpiresAt"`
	}
	_, err = deviceJSON(ctx, client, region, "/client/register", map[string]any{
		"clientName": "Kiro", "clientType": "public", "issuerUrl": portal,
		"scopes":     []string{"codewhisperer:completions", "codewhisperer:analysis", "codewhisperer:conversations", "codewhisperer:transformations", "codewhisperer:taskassist"},
		"grantTypes": []string{"urn:ietf:params:oauth:grant-type:device_code", "refresh_token"},
	}, &registered)
	if err != nil {
		return nil, err
	}
	if registered.ClientID == "" || registered.ClientSecret == "" || registered.ClientSecretExpiresAt <= time.Now().Unix() {
		return nil, fmt.Errorf("kiro: invalid registered client")
	}
	var device struct {
		DeviceCode  string `json:"deviceCode"`
		UserCode    string `json:"userCode"`
		URI         string `json:"verificationUri"`
		CompleteURI string `json:"verificationUriComplete"`
		Interval    int    `json:"interval"`
		ExpiresIn   int64  `json:"expiresIn"`
	}
	_, err = deviceJSON(ctx, client, region, "/device_authorization", map[string]string{"clientId": registered.ClientID, "clientSecret": registered.ClientSecret, "startUrl": portal}, &device)
	if err != nil {
		return nil, err
	}
	authURL := device.CompleteURI
	if authURL == "" {
		authURL = device.URI
	}
	if device.DeviceCode == "" || device.UserCode == "" || !validDeviceAuthURL(authURL) || device.ExpiresIn <= 0 || device.ExpiresIn > 1800 || device.Interval > 60 {
		return nil, fmt.Errorf("kiro: invalid device login response")
	}
	return &DeviceLogin{ClientID: registered.ClientID, ClientSecret: registered.ClientSecret, ClientSecretExpiresAt: registered.ClientSecretExpiresAt, DeviceCode: device.DeviceCode, UserCode: device.UserCode, AuthURL: authURL, Interval: max(5, device.Interval), ExpiresAt: min(time.Now().Unix()+device.ExpiresIn, registered.ClientSecretExpiresAt)}, nil
}

func PollDeviceLogin(ctx context.Context, client Doer, region string, login *DeviceLogin) (*DeviceToken, string, error) {
	var token DeviceToken
	status, err := deviceJSON(ctx, client, region, "/token", map[string]string{"clientId": login.ClientID, "clientSecret": login.ClientSecret, "deviceCode": login.DeviceCode, "grantType": "urn:ietf:params:oauth:grant-type:device_code"}, &token)
	if err != nil || status != "completed" {
		return nil, status, err
	}
	if token.AccessToken == "" || token.RefreshToken == "" || token.ExpiresIn <= 0 || token.ExpiresIn > 365*86400 {
		return nil, "", fmt.Errorf("kiro: device token response lacks credentials or expiry")
	}
	return &token, status, nil
}
