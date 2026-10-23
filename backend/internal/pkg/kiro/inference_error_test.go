package kiro

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestInferenceErrorSanitizesUpstreamDetails(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		code   string
	}{
		{429, `{"message":"secret-token user@example.com","reason":"unknown"}`, "kiro_throttled"},
		{402, `{"message":"secret-token"}`, "kiro_payment_required"},
		{401, `{"message":"secret-token"}`, "kiro_unauthorized"},
		{403, `{"message":"secret-token"}`, "kiro_forbidden"},
		{400, `{"reason":"INVALID_MODEL_ID","message":"secret-token"}`, "kiro_invalid_model"},
		{400, `{"reason":"secret-token"}`, "kiro_upstream_error"},
		{503, "secret-token", "kiro_unavailable"},
	} {
		e := ReadInferenceError(&http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))})
		if e.Code != tc.code || e.StatusCode != tc.status {
			t.Fatalf("unexpected classification: %+v", e)
		}
		if strings.Contains(e.Error(), "secret-token") || strings.Contains(e.Error(), "user@example.com") {
			t.Fatal("upstream details leaked")
		}
	}
}

func TestInferenceErrorBoundsBodyRead(t *testing.T) {
	body := strings.NewReader(strings.Repeat("x", 128<<10))
	ReadInferenceError(&http.Response{StatusCode: 429, Body: io.NopCloser(body)})
	if body.Len() < 64<<10 {
		t.Fatal("read more than the error body limit")
	}
}
