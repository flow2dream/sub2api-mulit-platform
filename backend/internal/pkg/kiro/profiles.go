package kiro

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// DiscoverProfile uses the AWS JSON operation defined by the public Q CLI SDK.
// Never choose arbitrarily among multiple profiles or search other regions.
func DiscoverProfile(ctx context.Context, client Doer, c Credentials) (string, error) {
	if c.AuthMethod == "api_key" {
		return "", fmt.Errorf("kiro: API Key does not use OAuth profiles")
	}
	profiles, tokens := map[string]bool{}, map[string]bool{}
	next := ""
	for page := 0; page < 20; page++ {
		body := map[string]any{"maxResults": 50}
		if next != "" {
			body["nextToken"] = next
		}
		data, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://codewhisperer."+c.APIRegion+".amazonaws.com/", bytes.NewReader(data))
		if err != nil {
			return "", err
		}
		Authorize(req, c)
		req.Header.Set("Content-Type", "application/x-amz-json-1.0")
		req.Header.Set("X-Amz-Target", "AmazonCodeWhispererService.ListAvailableProfiles")
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("kiro: profile discovery connection failed")
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return "", &HTTPError{StatusCode: resp.StatusCode}
		}
		if readErr != nil || len(data) > 1<<20 {
			return "", fmt.Errorf("kiro: invalid profile response")
		}
		var result struct {
			Profiles []struct {
				ARN string `json:"arn"`
			} `json:"profiles"`
			Next string `json:"nextToken"`
		}
		if json.Unmarshal(data, &result) != nil {
			return "", fmt.Errorf("kiro: invalid profile response")
		}
		for _, profile := range result.Profiles {
			parts := strings.SplitN(profile.ARN, ":", 6)
			if len(parts) != 6 || parts[0] != "arn" || parts[2] != "codewhisperer" || parts[3] != c.APIRegion || !strings.HasPrefix(parts[5], "profile/") || len(parts[5]) <= 8 {
				return "", fmt.Errorf("kiro: invalid profile region or ARN")
			}
			profiles[profile.ARN] = true
		}
		if result.Next == "" {
			if len(profiles) != 1 {
				return "", fmt.Errorf("kiro: select one profile from this account's Kiro credentials")
			}
			for arn := range profiles {
				return arn, nil
			}
		}
		if tokens[result.Next] {
			return "", fmt.Errorf("kiro: repeated profile page")
		}
		next = result.Next
		tokens[next] = true
	}
	return "", fmt.Errorf("kiro: profile pagination limit exceeded")
}
