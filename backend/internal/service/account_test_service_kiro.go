package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func (s *AccountTestService) RefreshKiroAccount(ctx context.Context, a *Account) (*Account, error) {
	if s == nil || s.kiroGateway == nil {
		return nil, fmt.Errorf("kiro gateway is not configured")
	}
	return s.kiroGateway.RefreshAccount(ctx, a)
}

// FetchKiroAccountModels discovers the native account catalog for the test picker.
// Account aliases are projected onto live targets without changing credentials.
func (s *AccountTestService) FetchKiroAccountModels(ctx context.Context, a *Account) ([]claude.Model, error) {
	if a == nil || a.Platform != PlatformKiro {
		return nil, newUpstreamModelSyncConfigError("A Kiro account is required", nil)
	}
	if a.Type == AccountTypeAPIKey {
		models := make([]claude.Model, 0)
		for id := range a.GetModelMapping() {
			if !strings.Contains(id, "*") {
				models = append(models, claude.Model{ID: id, Type: "model", DisplayName: id})
			}
		}
		sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
		if len(models) == 0 {
			return nil, newUpstreamModelSyncUnsupportedError("Kiro API Key model discovery is unavailable; configure model mappings", nil)
		}
		return models, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ids, body, err := s.fetchKiroUpstreamModels(ctx, a)
	if err != nil {
		return nil, err
	}
	var catalog struct {
		Models []struct {
			ID   string `json:"modelId"`
			Name string `json:"modelName"`
		} `json:"models"`
	}
	_ = json.Unmarshal(body, &catalog)
	names := make(map[string]string, len(catalog.Models))
	for _, model := range catalog.Models {
		names[model.ID] = strings.TrimSpace(model.Name)
	}
	live := make(map[string]bool, len(ids))
	for _, id := range ids {
		live[id] = true
	}
	candidates := append([]string{}, ids...)
	for id := range a.GetModelMapping() {
		if !strings.Contains(id, "*") {
			candidates = append(candidates, id)
		}
	}
	models := make([]claude.Model, 0, len(candidates))
	for _, id := range dedupeAndSortModelIDs(candidates) {
		target := kiro.NormalizeModelID(a.GetMappedModel(id))
		// Auto appears in the IDE catalog but was rejected on both tested
		// OAuth inference paths, including runtime with a valid Profile ARN.
		if target == "auto" {
			continue
		}
		if !a.IsModelSupported(id) || !live[target] {
			continue
		}
		name := names[target]
		if name == "" {
			name = target
		}
		if id != target {
			name = id + " (" + name + ")"
		}
		models = append(models, claude.Model{ID: id, Type: "model", DisplayName: name})
	}
	if len(models) == 0 {
		return nil, newUpstreamModelSyncConfigError("No discovered Kiro models match the account model mappings", nil)
	}
	return models, nil
}

func (s *AccountTestService) fetchKiroUpstreamModels(ctx context.Context, a *Account) ([]string, []byte, error) {
	if s.kiroGateway == nil {
		return nil, nil, newUpstreamModelSyncConfigError("Kiro gateway is not configured", nil)
	}
	fresh, err := s.kiroGateway.tokens.Account(ctx, a, false)
	if err != nil {
		return nil, nil, err
	}
	a = fresh
	req, err := s.buildKiroModelsRequest(ctx, a)
	if err != nil {
		return nil, nil, err
	}
	all := make([]json.RawMessage, 0)
	seen := map[string]bool{}
	for page := 0; page < 20; page++ {
		data, err := s.readUpstreamModelsResponse(req, a)
		if err != nil {
			return nil, nil, err
		}
		var result struct {
			Models    []json.RawMessage `json:"models"`
			NextToken string            `json:"nextToken"`
		}
		if json.Unmarshal(data, &result) != nil {
			return nil, nil, newUpstreamModelSyncUpstreamError("Invalid Kiro model response", nil)
		}
		all = append(all, result.Models...)
		if result.NextToken == "" {
			merged, _ := json.Marshal(map[string]any{"models": all})
			ids := make([]string, 0, len(all))
			for _, raw := range all {
				var model struct {
					ID string `json:"modelId"`
				}
				if json.Unmarshal(raw, &model) != nil {
					return nil, nil, newUpstreamModelSyncUpstreamError("Invalid Kiro model entry", nil)
				}
				ids = append(ids, model.ID)
			}
			ids = dedupeAndSortModelIDs(ids)
			if len(ids) == 0 {
				return nil, nil, newUpstreamModelSyncUpstreamError("Kiro returned no supported models", nil)
			}
			return ids, merged, nil
		}
		if seen[result.NextToken] {
			return nil, nil, newUpstreamModelSyncUpstreamError("Kiro model pagination repeated a token", nil)
		}
		seen[result.NextToken] = true
		q := req.URL.Query()
		q.Set("nextToken", result.NextToken)
		req.URL.RawQuery = q.Encode()
	}
	return nil, nil, newUpstreamModelSyncUpstreamError("Kiro model pagination exceeded its limit", nil)
}
func (s *AccountTestService) testKiroAccountConnection(c *gin.Context, a *Account, model, prompt, mode string) error {
	if s.kiroGateway == nil {
		return s.sendErrorAndEnd(c, "Kiro gateway is not configured")
	}
	if mode != "" && mode != "default" && mode != "text" {
		return s.sendErrorAndEnd(c, "Kiro supports text/tool connectivity tests only")
	}
	if _, busy := s.kiroTests.LoadOrStore(a.ID, struct{}{}); busy {
		return s.sendKiroTestError(c, "kiro_test_busy", "A Kiro connectivity test is already running for this account", 0)
	}
	defer s.kiroTests.Delete(a.ID)
	if a.TempUnschedulableUntil != nil && time.Now().Before(*a.TempUnschedulableUntil) {
		return s.sendKiroTestError(c, "kiro_cooldown", "Kiro account is cooling down; wait before retrying", time.Until(*a.TempUnschedulableUntil))
	}
	if model == "" {
		models, err := s.FetchKiroAccountModels(c.Request.Context(), a)
		if err != nil {
			return s.sendErrorAndEnd(c, "Failed to discover Kiro test models; check authorization, region and proxy")
		}
		// Prefer the concrete model verified with the IDE integration; the
		// catalog alone does not establish Claude inference entitlement.
		for _, candidate := range models {
			target := a.GetMappedModel(candidate.ID)
			if a.Type == AccountTypeOAuth && target == "auto" {
				continue
			}
			if model == "" || target == "minimax-m2.5" {
				model = candidate.ID
			}
			if target == "minimax-m2.5" {
				break
			}
		}
		if model == "" {
			return s.sendKiroTestError(c, "kiro_invalid_model", "Kiro returned no concrete model for a connectivity test", 0)
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	a, err := s.kiroGateway.tokens.Account(ctx, a, false)
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	if a.TempUnschedulableUntil != nil && time.Now().Before(*a.TempUnschedulableUntil) {
		return s.sendKiroTestError(c, "kiro_cooldown", "Kiro account is cooling down; wait before retrying", time.Until(*a.TempUnschedulableUntil))
	}
	if a.Type == AccountTypeOAuth && a.GetMappedModel(model) == "auto" {
		return s.sendKiroTestError(c, "kiro_auto_endpoint", "Auto is not supported by this Kiro OAuth integration; select a concrete model", 0)
	}
	credentials, err := s.kiroGateway.inferenceCredentials(ctx, a)
	if err != nil {
		if err == errKiroRuntimeProfile {
			return s.sendKiroTestError(c, "kiro_runtime_profile", err.Error(), 0)
		}
		return s.sendErrorAndEnd(c, err.Error())
	}
	if prompt == "" {
		prompt = "hi"
	}
	content, _ := json.Marshal(prompt)
	payload, err := kiro.BuildRequest(&apicompat.AnthropicRequest{Model: a.GetMappedModel(model), MaxTokens: 128, Messages: []apicompat.AnthropicMessage{{Role: "user", Content: content}}}, credentials, uuid.NewString())
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	req, err := kiro.NewInferenceRequest(ctx, credentials, payload)
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	s.sendEvent(c, TestEvent{Type: "status", Code: "kiro_connecting", Text: "Sending Kiro inference request; waiting for upstream acceptance"})
	resp, err := (kiroAccountClient{s.httpUpstream, a}).Do(req)
	if err != nil {
		return s.sendErrorAndEnd(c, "Kiro transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		failure := kiro.ReadInferenceError(resp)
		delay := s.kiroGateway.observeHTTPError(ctx, a, resp.StatusCode, resp.Header)
		return s.sendKiroTestError(c, failure.Code, failure.Error(), delay)
	}
	s.sendEvent(c, TestEvent{Type: "test_start", Model: model})
	_, err = kiro.Decode(resp.Body, "test_"+uuid.NewString(), model, 0, payload.ToolNames, func(event apicompat.AnthropicStreamEvent) error {
		if event.Delta != nil && event.Delta.Text != "" {
			s.sendEvent(c, TestEvent{Type: "content", Text: event.Delta.Text})
		}
		return nil
	})
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

func (s *AccountTestService) sendKiroTestError(c *gin.Context, code, message string, delay time.Duration) error {
	logAccountTestError(c, message)
	s.sendEvent(c, TestEvent{Type: "error", Code: code, Error: message, Data: map[string]any{"retry_after_seconds": int(math.Ceil(delay.Seconds()))}})
	return fmt.Errorf("%s", message)
}

func (s *AccountTestService) buildKiroModelsRequest(ctx context.Context, a *Account) (*http.Request, error) {
	if s.kiroGateway == nil {
		return nil, newUpstreamModelSyncConfigError("Kiro gateway is not configured", nil)
	}
	fresh, err := s.kiroGateway.tokens.Account(ctx, a, false)
	if err != nil {
		return nil, newUpstreamModelSyncConfigError("Kiro credentials unavailable", err)
	}
	credentials, err := kiro.Parse(fresh.Credentials)
	if err != nil {
		return nil, newUpstreamModelSyncConfigError("Invalid Kiro credentials", err)
	}
	req, err := kiro.NewManagementRequest(ctx, credentials, "models", "")
	if err != nil {
		return nil, newUpstreamModelSyncUnsupportedError(err.Error(), err)
	}
	return req, nil
}
