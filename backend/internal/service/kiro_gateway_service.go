package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/kiro"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	gocache "github.com/patrickmn/go-cache"
	"github.com/tidwall/gjson"
	"golang.org/x/sync/singleflight"
)

type KiroGatewayService struct {
	upstream    HTTPUpstream
	tokens      *KiroTokenProvider
	usageRepo   KiroUsageRepository
	usageCache  *gocache.Cache
	usageFlight singleflight.Group
}

// KiroUsageRepository stores native metering independently of token billing.
type KiroUsageRepository interface {
	RecordKiroUsage(context.Context, string, int64, int64, string, kiro.Usage) error
}

func ProvideKiroGatewayService(repo AccountRepository, upstream HTTPUpstream, refreshAPI *OAuthRefreshAPI, invalidator TokenCacheInvalidator, usageRepo UsageLogRepository) *KiroGatewayService {
	r := &KiroTokenRefresher{upstream: upstream}
	s := &KiroGatewayService{upstream: upstream, tokens: &KiroTokenProvider{repo: repo, refreshAPI: refreshAPI, refresher: r, invalidator: invalidator}, usageCache: newKiroUsageCache()}
	s.usageRepo, _ = usageRepo.(KiroUsageRepository)
	return s
}

// RefreshAccount is shared by admin single/batch refresh and request refresh.
func (s *KiroGatewayService) RefreshAccount(ctx context.Context, a *Account) (*Account, error) {
	return s.tokens.Account(ctx, a, true)
}

func (s *KiroGatewayService) Forward(ctx context.Context, c *gin.Context, a *Account, body []byte, protocol string) (*ForwardResult, error) {
	start := time.Now()
	req, mapping, err := s.normalize(body, protocol)
	if err != nil {
		kiroWriteError(c, protocol, 400, err.Error())
		return nil, err
	}
	originalModel := req.Model
	a, err = s.tokens.Account(ctx, a, false)
	if err != nil {
		return nil, &UpstreamFailoverError{StatusCode: 503, ClientMessage: "Kiro credentials unavailable"}
	}
	req.Model = kiro.NormalizeModelID(a.GetMappedModel(originalModel))
	if req.Model == "auto" && a.Type == AccountTypeOAuth {
		err = fmt.Errorf("kiro: Auto is not supported by this OAuth integration; select a concrete model")
		kiroWriteError(c, protocol, 400, err.Error())
		return nil, err
	}
	credentials, err := s.inferenceCredentials(ctx, a)
	if err != nil {
		kiroWriteError(c, protocol, 400, err.Error())
		return nil, err
	}
	// Each attempt sends complete history under an independent UUID. Kiro's
	// native API does not need a shared server-side session for stateless calls.
	payload, err := kiro.BuildRequest(req, credentials, uuid.NewString())
	if err != nil {
		kiroWriteError(c, protocol, 400, err.Error())
		return nil, err
	}
	encoded, _ := json.Marshal(payload)
	inputEstimate := kiro.EstimateTokens(string(encoded))
	upstreamCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var resp *http.Response
	for attempt := 0; attempt < 2; attempt++ {
		upstreamReq, buildErr := kiro.NewInferenceRequest(upstreamCtx, credentials, payload)
		if buildErr != nil {
			return nil, buildErr
		}
		resp, err = (kiroAccountClient{s.upstream, a}).Do(upstreamReq)
		if err != nil {
			return nil, &UpstreamFailoverError{StatusCode: 502, ClientMessage: "Kiro transport failed"}
		}
		if resp.StatusCode != 401 || a.Type != AccountTypeOAuth || attempt != 0 {
			break
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		a, err = s.tokens.Account(ctx, a, true)
		if err != nil {
			return nil, &UpstreamFailoverError{StatusCode: 401, ClientMessage: "Kiro requires reauthorization"}
		}
		credentials, err = s.inferenceCredentials(ctx, a)
		if err != nil {
			return nil, err
		}
		// Refresh may discover a profile; rebuild the entire account attempt.
		payload, err = kiro.BuildRequest(req, credentials, payload.ConversationState.ConversationID)
		if err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		failure := kiro.ReadInferenceError(resp)
		s.observeHTTPError(ctx, a, resp.StatusCode, resp.Header)
		if resp.StatusCode == 401 || resp.StatusCode == 402 || resp.StatusCode == 403 || resp.StatusCode == 429 || resp.StatusCode >= 500 {
			return nil, &UpstreamFailoverError{StatusCode: resp.StatusCode, ResponseHeaders: resp.Header, ClientMessage: failure.Error()}
		}
		status := resp.StatusCode
		if status < 400 || status > 499 {
			status = 502
		}
		err = failure
		kiroWriteError(c, protocol, status, err.Error())
		return nil, err
	}
	requestID := resp.Header.Get("x-amzn-requestid")
	if requestID == "" {
		requestID = uuid.NewString()
	}
	c.Header("X-Sub2API-Usage-Source", "estimated-unless-reported")
	encoder := newKiroOutput(c, protocol, req.Stream, mapping)
	summary, decodeErr := kiro.Decode(resp.Body, "msg_"+uuid.NewString(), originalModel, inputEstimate, payload.ToolNames, encoder.emit)
	result := &ForwardResult{RequestID: requestID, UpstreamHeaders: resp.Header, Model: originalModel, UpstreamModel: req.Model, Stream: req.Stream, Duration: time.Since(start), Usage: ClaudeUsage{InputTokens: summary.Usage.InputTokens, OutputTokens: summary.Usage.OutputTokens}, UsageSource: summary.Usage.Source, UpstreamUsage: &summary.Usage}
	if encoder.firstToken != nil {
		ms := int(encoder.firstToken.Sub(start).Milliseconds())
		result.FirstTokenMs = &ms
	}
	if s.usageRepo != nil {
		persistCtx, persistCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		apiKeyID := int64(0)
		if value, ok := c.Get("api_key"); ok {
			if key, ok := value.(*APIKey); ok && key != nil {
				apiKeyID = key.ID
			}
		}
		persistErr := s.usageRepo.RecordKiroUsage(persistCtx, resolveUsageBillingRequestID(ctx, requestID), a.ID, apiKeyID, req.Model, summary.Usage)
		persistCancel()
		if persistErr != nil {
			decodeErr = errors.Join(decodeErr, fmt.Errorf("kiro: failed to persist upstream metering: %w", persistErr))
		}
	}
	if decodeErr != nil {
		if ctx.Err() != nil {
			result.ClientDisconnect = true
		}
		if encoder.started {
			encoder.writeError("Kiro stream interrupted or invalid")
		} else {
			// Observed consumption must not disappear into an automatic retry.
			if summary.Usage.Credits == nil && !summary.Usage.InputReported && !summary.Usage.OutputReported && len(summary.Response.Content) == 0 {
				return nil, &UpstreamFailoverError{StatusCode: 502, ClientMessage: "Kiro stream ended before output"}
			}
			kiroWriteError(c, protocol, 502, "Kiro stream interrupted or invalid")
		}
		return result, decodeErr
	}
	if !req.Stream {
		c.Header("X-Sub2API-Usage-Source", summary.Usage.Source)
		if err = encoder.writeResponse(&summary.Response); err != nil {
			return result, err
		}
	} else {
		if err = encoder.finish(); err != nil {
			return result, err
		}
		if protocol == APIProtocolChatCompletions {
			_, err = c.Writer.WriteString("data: [DONE]\n\n")
			c.Writer.Flush()
		}
	}
	return result, err
}

func (s *KiroGatewayService) normalize(body []byte, protocol string) (*apicompat.AnthropicRequest, apicompat.ResponsesClientToolMapping, error) {
	var mapping apicompat.ResponsesClientToolMapping
	var req apicompat.AnthropicRequest
	if !json.Valid(body) || !gjson.ParseBytes(body).IsObject() {
		return nil, mapping, fmt.Errorf("kiro: request must be a JSON object")
	}
	for _, field := range []string{"reasoning.effort", "reasoning_effort"} {
		if v := gjson.GetBytes(body, field); v.Exists() && v.Type != gjson.Null && v.String() != "" {
			return nil, mapping, fmt.Errorf("kiro: explicit reasoning controls are unsupported")
		}
	}
	if v := gjson.GetBytes(body, "parallel_tool_calls"); v.Exists() && v.Type != gjson.Null && !v.Bool() {
		return nil, mapping, fmt.Errorf("kiro: disabling parallel tool calls is unsupported")
	}
	switch protocol {
	case APIProtocolAnthropic:
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, mapping, fmt.Errorf("kiro: invalid Messages request")
		}
	case APIProtocolResponses:
		if gjson.GetBytes(body, "store").Bool() || gjson.GetBytes(body, "background").Bool() {
			return nil, mapping, fmt.Errorf("kiro: stored/background Responses are unsupported")
		}
		if gjson.GetBytes(body, "previous_response_id").String() != "" {
			return nil, mapping, fmt.Errorf("kiro: previous_response_id is unsupported; send complete history")
		}
		if format := gjson.GetBytes(body, "text.format.type").String(); format != "" && format != "text" {
			return nil, mapping, fmt.Errorf("kiro: structured output is not supported")
		}
		adapted, m, err := adaptResponsesClientToolsForAnthropic(body)
		if err != nil {
			return nil, mapping, err
		}
		mapping = m
		adapted, err = prepareKiroResponsesInput(adapted)
		if err != nil {
			return nil, mapping, err
		}
		var responses apicompat.ResponsesRequest
		if err = json.Unmarshal(adapted, &responses); err != nil {
			return nil, mapping, err
		}
		model := responses.Model
		responses.Model = "kiro-native"
		converted, err := apicompat.ResponsesToAnthropicRequest(&responses)
		if err != nil {
			return nil, mapping, err
		}
		req = *converted
		req.Model = model
	case APIProtocolChatCompletions:
		if err := validateKiroChatInput(body); err != nil {
			return nil, mapping, err
		}
		if v := gjson.GetBytes(body, "stop"); v.Exists() && v.Type != gjson.Null && v.Raw != "[]" && v.String() != "" {
			return nil, mapping, fmt.Errorf("kiro: stop sequences are unsupported")
		}
		if format := gjson.GetBytes(body, "response_format.type").String(); format != "" && format != "text" {
			return nil, mapping, fmt.Errorf("kiro: structured output is not supported")
		}
		if n := gjson.GetBytes(body, "n").Int(); n > 1 {
			return nil, mapping, fmt.Errorf("kiro: only n=1 is supported")
		}
		var cc apicompat.ChatCompletionsRequest
		if err := json.Unmarshal(body, &cc); err != nil {
			return nil, mapping, err
		}
		clientModel := cc.Model
		cc.Model = "kiro-native"
		// Reuse structural conversion without OpenAI model policies or a token floor.
		responses, err := apicompat.ChatCompletionsToResponsesWithMinOutputTokens(&cc, 1)
		if err != nil {
			return nil, mapping, err
		}
		model := clientModel
		responses.Model = "kiro-native"
		converted, err := apicompat.ResponsesToAnthropicRequest(responses)
		if err != nil {
			return nil, mapping, err
		}
		req = *converted
		req.Model = model
	default:
		return nil, mapping, fmt.Errorf("kiro: unsupported client protocol")
	}
	// Conversion helpers may force upstream streaming. The client decides the
	// downstream encoding independently of Kiro's always-streaming upstream.
	req.Stream = gjson.GetBytes(body, "stream").Bool()
	return &req, mapping, nil
}

func kiroWriteError(c *gin.Context, protocol string, status int, message string) {
	if protocol == APIProtocolAnthropic {
		c.JSON(status, gin.H{"type": "error", "error": gin.H{"type": "invalid_request_error", "message": message}})
	} else {
		c.JSON(status, gin.H{"error": gin.H{"type": "invalid_request_error", "message": message}})
	}
}

type kiroOutput struct {
	c               *gin.Context
	protocol        string
	stream, started bool
	firstToken      *time.Time
	responses       *apicompat.AnthropicEventToResponsesState
	chat            *apicompat.ResponsesEventToChatState
	restorer        *apicompat.ResponsesClientToolStreamRestorer
	mapping         apicompat.ResponsesClientToolMapping
	toolEvents      map[int][]apicompat.AnthropicStreamEvent
	indexes         map[int]int
	nextIndex       int
	finalEvents     []apicompat.AnthropicStreamEvent
}

func newKiroOutput(c *gin.Context, protocol string, stream bool, mapping apicompat.ResponsesClientToolMapping) *kiroOutput {
	return &kiroOutput{c: c, protocol: protocol, stream: stream, responses: apicompat.NewAnthropicEventToResponsesState(), chat: apicompat.NewResponsesEventToChatState(), restorer: apicompat.NewResponsesClientToolStreamRestorer(mapping), mapping: mapping, toolEvents: map[int][]apicompat.AnthropicStreamEvent{}, indexes: map[int]int{}}
}
func (o *kiroOutput) emit(event apicompat.AnthropicStreamEvent) error {
	if !o.stream {
		return nil
	}
	if event.Type == "message_delta" || event.Type == "message_stop" {
		o.finalEvents = append(o.finalEvents, event)
		return nil
	}
	// The existing Responses converter tracks one current item. Serialize only
	// tool blocks (after complete JSON) so interleaved native calls cannot cross
	// their IDs or arguments. Text remains incremental.
	if event.Index != nil {
		old := *event.Index
		idx := old
		event.Index = &idx
		if event.Type == "content_block_start" && event.ContentBlock != nil && event.ContentBlock.Type == "tool_use" {
			o.toolEvents[old] = []apicompat.AnthropicStreamEvent{event}
			return nil
		}
		if pending, ok := o.toolEvents[old]; ok {
			if event.Delta != nil {
				d := *event.Delta
				event.Delta = &d
			}
			pending = append(pending, event)
			o.toolEvents[old] = pending
			if event.Type != "content_block_stop" {
				return nil
			}
			delete(o.toolEvents, old)
			index := o.nextIndex
			o.nextIndex++
			for _, e := range pending {
				i := index
				e.Index = &i
				if err := o.encode(e); err != nil {
					return err
				}
			}
			return nil
		}
		if event.Type == "content_block_start" {
			o.indexes[old] = o.nextIndex
			o.nextIndex++
		}
		index := o.indexes[old]
		event.Index = &index
	}
	return o.encode(event)
}
func (o *kiroOutput) finish() error {
	for _, e := range o.finalEvents {
		if err := o.encode(e); err != nil {
			return err
		}
	}
	return nil
}
func (o *kiroOutput) encode(event apicompat.AnthropicStreamEvent) error {
	if o.protocol == APIProtocolAnthropic {
		return o.writeSSE(event.Type, event)
	}
	for _, response := range apicompat.AnthropicEventToResponsesEvents(&event, o.responses) {
		if o.protocol == APIProtocolResponses {
			for _, restored := range o.restorer.Restore(response) {
				if err := o.writeSSE(restored.Type, restored); err != nil {
					return err
				}
			}
		} else {
			for _, chunk := range apicompat.ResponsesEventToChatChunks(&response, o.chat) {
				if err := o.writeSSE("", chunk); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func (o *kiroOutput) writeSSE(event string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if !o.started {
		o.started = true
		now := time.Now()
		o.firstToken = &now
		o.c.Header("Content-Type", "text/event-stream")
		o.c.Header("Cache-Control", "no-cache")
		o.c.Header("X-Accel-Buffering", "no")
	}
	if event != "" {
		if _, err = o.c.Writer.WriteString("event: " + event + "\n"); err != nil {
			return err
		}
	}
	_, err = o.c.Writer.WriteString("data: " + string(data) + "\n\n")
	o.c.Writer.Flush()
	return err
}
func (o *kiroOutput) writeError(message string) {
	if o.protocol == APIProtocolResponses {
		_ = o.writeSSE("error", gin.H{"type": "error", "code": "upstream_error", "message": message})
	} else if o.protocol == APIProtocolAnthropic {
		_ = o.writeSSE("error", gin.H{"type": "error", "error": gin.H{"type": "api_error", "message": message}})
	} else {
		_ = o.writeSSE("", gin.H{"error": gin.H{"type": "upstream_error", "message": message}})
	}
}
func (o *kiroOutput) writeResponse(message *apicompat.AnthropicResponse) error {
	if o.protocol == APIProtocolAnthropic {
		o.c.JSON(200, message)
		return nil
	}
	response := apicompat.AnthropicToResponsesResponse(message)
	if o.protocol == APIProtocolChatCompletions {
		o.c.JSON(200, apicompat.ResponsesToChatCompletions(response, message.Model))
		return nil
	}
	data, err := json.Marshal(response)
	if err != nil {
		return err
	}
	data, _, err = apicompat.RestoreResponsesClientToolPayload(data, o.mapping)
	if err != nil {
		return err
	}
	o.c.Data(200, "application/json; charset=utf-8", data)
	return nil
}
