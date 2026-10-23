package kiro

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// InferenceError exposes only local, allowlisted descriptions. Native error
// bodies and headers can contain account details and must not reach clients.
type InferenceError struct {
	StatusCode int
	Code       string
	message    string
}

func (e *InferenceError) Error() string { return e.message }

// ReadInferenceError consumes at most 64 KiB, without retaining raw messages.
func ReadInferenceError(resp *http.Response) *InferenceError {
	e := &InferenceError{StatusCode: resp.StatusCode, Code: "kiro_upstream_error", message: fmt.Sprintf("Kiro upstream HTTP %d", resp.StatusCode)}
	var body struct {
		Reason string `json:"reason"`
	}
	if resp.Body != nil {
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body)
	}
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		e.Code, e.message = "kiro_throttled", "Kiro is temporarily throttling inference requests (HTTP 429); this response does not establish quota exhaustion"
	case http.StatusPaymentRequired:
		e.Code, e.message = "kiro_payment_required", "Kiro requires an account quota or subscription check (HTTP 402)"
	case http.StatusUnauthorized:
		e.Code, e.message = "kiro_unauthorized", "Kiro authorization has expired or is invalid (HTTP 401)"
	case http.StatusForbidden:
		e.Code, e.message = "kiro_forbidden", "Kiro denied inference access; check account permissions, subscription and region (HTTP 403)"
	case http.StatusBadRequest:
		if body.Reason == "INVALID_MODEL_ID" {
			e.Code, e.message = "kiro_invalid_model", "Kiro rejected this model for inference; select another available model (HTTP 400)"
		}
	default:
		if resp.StatusCode >= 500 {
			e.Code, e.message = "kiro_unavailable", fmt.Sprintf("Kiro inference service is temporarily unavailable (HTTP %d)", resp.StatusCode)
		}
	}
	return e
}
