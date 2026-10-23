package kiro

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKiroDeviceLoginRegistersClientAndReturnsAWSVerificationLink(t *testing.T) {
	calls := 0
	client := doerFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, "oidc.us-west-2.amazonaws.com", req.URL.Host)
		require.Equal(t, "application/json", req.Header.Get("Content-Type"))
		var body map[string]any
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		var data string
		if req.URL.Path == "/client/register" {
			require.Equal(t, BuilderIDStartURL, body["issuerUrl"])
			require.Equal(t, "public", body["clientType"])
			require.Contains(t, body["scopes"], "codewhisperer:completions")
			data = fmt.Sprintf(`{"clientId":"client","clientSecret":"secret","clientSecretExpiresAt":%d}`, time.Now().Add(time.Hour).Unix())
		} else {
			require.Equal(t, "/device_authorization", req.URL.Path)
			require.Equal(t, "secret", body["clientSecret"])
			data = `{"deviceCode":"device","userCode":"USER-CODE","verificationUriComplete":"https://view.awsapps.com/start/#/device","expiresIn":600,"interval":2}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(data))}, nil
	})
	login, err := StartDeviceLogin(context.Background(), client, "us-west-2", "")
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	require.Equal(t, 5, login.Interval)
	require.Equal(t, "device", login.DeviceCode)
	require.Equal(t, "USER-CODE", login.UserCode)
}

func TestKiroDeviceTokenStatusesAndSecretFreeErrors(t *testing.T) {
	for _, tc := range []struct{ code, status string }{{"authorization_pending", "pending"}, {"slow_down", "slow_down"}, {"access_denied", "denied"}, {"expired_token", "expired"}, {"AuthorizationPendingException", "pending"}, {"invalid_grant", ""}} {
		t.Run(tc.code, func(t *testing.T) {
			client := doerFunc(func(req *http.Request) (*http.Response, error) {
				require.Equal(t, "/token", req.URL.Path)
				var body map[string]string
				require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
				require.Equal(t, "urn:ietf:params:oauth:grant-type:device_code", body["grantType"])
				return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"error":%q,"message":"do-not-expose-secret"}`, tc.code)))}, nil
			})
			_, status, err := PollDeviceLogin(context.Background(), client, "us-east-1", &DeviceLogin{ClientID: "client", ClientSecret: "secret", DeviceCode: "device"})
			require.Equal(t, tc.status, status)
			if tc.status == "" {
				require.Error(t, err)
				require.NotContains(t, err.Error(), "do-not-expose-secret")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestKiroDeviceLoginRejectsUnsafeOrMalformedInputs(t *testing.T) {
	for _, raw := range []string{"http://view.awsapps.com/start", "https://view.awsapps.com.evil.test/start", "https://user@view.awsapps.com/start", "https://view.awsapps.com:444/start", "https://view.awsapps.com/start?next=evil"} {
		_, err := ValidateDeviceStartURL(raw)
		require.Error(t, err)
	}
	_, err := StartDeviceLogin(context.Background(), doerFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("must not contact invalid region")
		return nil, nil
	}), "us-east-1.evil.test", "")
	require.Error(t, err)
	for _, data := range []string{`{"accessToken":"a","expiresIn":60}`, `{"accessToken":"a","refreshToken":"r","expiresIn":0}`, `{"accessToken":"a","refreshToken":"r","expiresIn":999999999}`} {
		_, _, err = PollDeviceLogin(context.Background(), doerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(data))}, nil
		}), "us-east-1", &DeviceLogin{})
		require.Error(t, err)
	}
	for _, raw := range []string{"javascript:alert(1)", "https://attacker.test/login", "https://view.awsapps.com.attacker.test/"} {
		require.False(t, validDeviceAuthURL(raw))
	}
}
