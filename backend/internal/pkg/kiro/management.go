package kiro

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Management is kept separate from runtime inference: API-key access to these
// IDE operations is not assumed. No hidden refresh or profile discovery occurs.
func NewManagementRequest(ctx context.Context, c Credentials, operation, nextToken string) (*http.Request, error) {
	if c.AuthMethod == "api_key" {
		return nil, fmt.Errorf("kiro: API Key management APIs have not been verified")
	}
	query := url.Values{"origin": {"AI_EDITOR"}}
	path := ""
	switch operation {
	case "models":
		path = "ListAvailableModels"
		query.Set("maxResults", "50")
	case "usage":
		path = "getUsageLimits"
		query.Set("resourceType", "AGENTIC_REQUEST")
		query.Set("isEmailRequired", "false")
	default:
		return nil, fmt.Errorf("kiro: unknown management operation")
	}
	if c.ProfileARN != "" {
		query.Set("profileArn", c.ProfileARN)
	}
	if nextToken != "" {
		query.Set("nextToken", nextToken)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "https://codewhisperer."+c.APIRegion+".amazonaws.com/"+path+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	Authorize(req, c)
	return req, nil
}

// Keep base/trial/bonus buckets and their expiries separate; an unknown bucket
// is not proof that the account is exhausted.
type UsageLimits struct {
	Breakdown []UsageBucket `json:"usageBreakdownList"`
	NextReset json.Number   `json:"nextDateReset"`
}
type UsageBucket struct {
	ResourceType string       `json:"resourceType"`
	Used         float64      `json:"currentUsage"`
	Limit        float64      `json:"usageLimit"`
	Unit         string       `json:"unit"`
	Trial        *TrialUsage  `json:"freeTrialInfo,omitempty"`
	Bonuses      []BonusUsage `json:"bonuses,omitempty"`
}
type TrialUsage struct {
	Used      float64     `json:"currentUsage"`
	Limit     float64     `json:"usageLimit"`
	Status    string      `json:"freeTrialStatus"`
	ExpiresAt json.Number `json:"freeTrialExpiry"`
}
type BonusUsage struct {
	Used      float64     `json:"currentUsage"`
	Limit     float64     `json:"usageLimit"`
	Status    string      `json:"status"`
	ExpiresAt json.Number `json:"expiresAt"`
	Name      string      `json:"displayName"`
}
