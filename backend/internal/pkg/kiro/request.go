package kiro

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/google/uuid"
)

type Payload struct {
	ConversationState Conversation      `json:"conversationState"`
	ProfileARN        string            `json:"profileArn,omitempty"`
	InferenceConfig   *InferenceConfig  `json:"inferenceConfig,omitempty"`
	ToolNames         map[string]string `json:"-"`
}
type Conversation struct {
	AgentContinuationID string           `json:"agentContinuationId,omitempty"`
	AgentTaskType       string           `json:"agentTaskType,omitempty"`
	ChatTriggerType     string           `json:"chatTriggerType"`
	ConversationID      string           `json:"conversationId"`
	CurrentMessage      HistoryMessage   `json:"currentMessage"`
	History             []HistoryMessage `json:"history,omitempty"`
}
type HistoryMessage struct {
	User      *UserMessage      `json:"userInputMessage,omitempty"`
	Assistant *AssistantMessage `json:"assistantResponseMessage,omitempty"`
}
type UserMessage struct {
	Content string       `json:"content"`
	ModelID string       `json:"modelId,omitempty"`
	Origin  string       `json:"origin"`
	Context *UserContext `json:"userInputMessageContext,omitempty"`
}
type UserContext struct {
	Tools       []Tool       `json:"tools,omitempty"`
	ToolResults []ToolResult `json:"toolResults,omitempty"`
}
type Tool struct {
	Specification ToolSpec `json:"toolSpecification"`
}
type ToolSpec struct {
	Name        string                     `json:"name"`
	Description string                     `json:"description"`
	InputSchema map[string]json.RawMessage `json:"inputSchema"`
}
type ToolResult struct {
	ID      string          `json:"toolUseId"`
	Content []ResultContent `json:"content"`
	Status  string          `json:"status"`
}
type ResultContent struct {
	Text string `json:"text"`
}
type AssistantMessage struct {
	Content  string    `json:"content"`
	ToolUses []ToolUse `json:"toolUses,omitempty"`
}
type ToolUse struct {
	ID    string          `json:"toolUseId"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}
type InferenceConfig struct {
	MaxTokens   int      `json:"maxTokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"topP,omitempty"`
}

var invalidToolChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func toolName(original string) string {
	clean := invalidToolChars.ReplaceAllString(original, "_")
	if len(clean) <= 64 && clean == original && clean != "" {
		return clean
	}
	h := sha256.Sum256([]byte(original))
	if len(clean) > 48 {
		clean = clean[:48]
	}
	return clean + "_" + hex.EncodeToString(h[:6])
}

// BuildRequest never modifies the normalized client request; every account
// attempt gets a fresh body, model mapping, origin and profile.
func BuildRequest(req *apicompat.AnthropicRequest, c Credentials, conversationID string) (*Payload, error) {
	if req == nil || req.Model == "" || len(req.Messages) == 0 {
		return nil, fmt.Errorf("kiro: model and messages are required")
	}
	if len(req.StopSeqs) != 0 {
		return nil, fmt.Errorf("kiro: stop_sequences are not supported")
	}
	if req.Thinking != nil && req.Thinking.Type != "disabled" {
		return nil, fmt.Errorf("kiro: explicit thinking controls are not supported yet")
	}
	if req.OutputConfig != nil && req.OutputConfig.Effort != "" {
		return nil, fmt.Errorf("kiro: reasoning effort controls are not supported yet")
	}
	if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		var choice struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(req.ToolChoice, &choice) != nil || choice.Type != "auto" {
			return nil, fmt.Errorf("kiro: only automatic tool choice is supported")
		}
	}
	origin := "AI_EDITOR"
	if c.EndpointFamily == "runtime" && c.AuthMethod == "api_key" {
		origin = "KIRO_CLI"
	}
	p := &Payload{ToolNames: map[string]string{}, InferenceConfig: &InferenceConfig{MaxTokens: req.MaxTokens, Temperature: req.Temperature, TopP: req.TopP}}
	p.ConversationState = Conversation{ChatTriggerType: "MANUAL", ConversationID: conversationID}
	if c.EndpointFamily == "q" && c.AuthMethod != "api_key" {
		p.ConversationState.AgentContinuationID = uuid.NewString()
		p.ConversationState.AgentTaskType = "vibe"
	}
	if c.AuthMethod != "api_key" {
		p.ProfileARN = c.ProfileARN
	}
	var tools []Tool
	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "custom" || t.Name == "" {
			return nil, fmt.Errorf("kiro: only named function tools are supported")
		}
		name := toolName(t.Name)
		if _, duplicate := p.ToolNames[name]; duplicate {
			return nil, fmt.Errorf("kiro: duplicate tool name")
		}
		p.ToolNames[name] = t.Name
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		var obj map[string]any
		if json.Unmarshal(schema, &obj) != nil || obj == nil {
			return nil, fmt.Errorf("kiro: invalid tool input schema")
		}
		tools = append(tools, Tool{ToolSpec{Name: name, Description: t.Description, InputSchema: map[string]json.RawMessage{"json": schema}}})
	}
	var history []HistoryMessage
	for _, m := range req.Messages {
		blocks, err := contentBlocks(m.Content)
		if err != nil {
			return nil, err
		}
		switch m.Role {
		case "user":
			u := &UserMessage{ModelID: NormalizeModelID(req.Model), Origin: origin, Context: &UserContext{}}
			for _, b := range blocks {
				switch b.Type {
				case "text":
					u.Content = joinText(u.Content, b.Text)
				case "tool_result":
					if b.ToolUseID == "" {
						return nil, fmt.Errorf("kiro: tool_result requires tool_use_id")
					}
					text, err := textContent(b.Content)
					if err != nil {
						return nil, err
					}
					status := "success"
					if b.IsError {
						status = "error"
					}
					u.Context.ToolResults = append(u.Context.ToolResults, ToolResult{ID: b.ToolUseID, Content: []ResultContent{{text}}, Status: status})
				default:
					return nil, fmt.Errorf("kiro: unsupported user content type %q", b.Type)
				}
			}
			if len(history) > 0 && history[len(history)-1].User != nil {
				previous := history[len(history)-1].User
				previous.Content = joinText(previous.Content, u.Content)
				previous.Context.ToolResults = append(previous.Context.ToolResults, u.Context.ToolResults...)
			} else {
				history = append(history, HistoryMessage{User: u})
			}
		case "assistant":
			a := &AssistantMessage{}
			for _, b := range blocks {
				switch b.Type {
				case "text":
					a.Content = joinText(a.Content, b.Text)
				case "thinking":
					// Kiro has no separate historical reasoning field. Preserve
					// visible reasoning as assistant history text; signatures
					// belong to the originating provider and are not reusable.
					a.Content = joinText(a.Content, b.Thinking)
				case "tool_use":
					var obj map[string]any
					if b.ID == "" || b.Name == "" || json.Unmarshal(b.Input, &obj) != nil || obj == nil {
						return nil, fmt.Errorf("kiro: invalid historical tool use")
					}
					a.ToolUses = append(a.ToolUses, ToolUse{ID: b.ID, Name: toolName(b.Name), Input: b.Input})
				default:
					return nil, fmt.Errorf("kiro: unsupported assistant content type %q", b.Type)
				}
			}
			if len(history) == 0 || history[len(history)-1].Assistant != nil {
				return nil, fmt.Errorf("kiro: assistant must follow a user message")
			}
			history = append(history, HistoryMessage{Assistant: a})
		default:
			return nil, fmt.Errorf("kiro: unsupported message role")
		}
	}
	if len(history) == 0 || history[len(history)-1].User == nil {
		return nil, fmt.Errorf("kiro: assistant prefill is not supported; last message must be user/tool results")
	}
	if len(req.System) > 0 && string(req.System) != "null" {
		system, err := textContent(req.System)
		if err != nil {
			return nil, err
		}
		history[0].User.Content = joinText(system, history[0].User.Content)
	}
	last := history[len(history)-1]
	last.User.Context.Tools = tools
	p.ConversationState.CurrentMessage, p.ConversationState.History = last, history[:len(history)-1]
	return p, nil
}

func contentBlocks(raw json.RawMessage) ([]apicompat.AnthropicContentBlock, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []apicompat.AnthropicContentBlock{{Type: "text", Text: text}}, nil
	}
	var blocks []apicompat.AnthropicContentBlock
	if json.Unmarshal(raw, &blocks) != nil || blocks == nil {
		return nil, fmt.Errorf("kiro: invalid content")
	}
	return blocks, nil
}
func textContent(raw json.RawMessage) (string, error) {
	blocks, err := contentBlocks(raw)
	if err != nil {
		return "", err
	}
	text := ""
	for _, b := range blocks {
		if b.Type != "text" {
			return "", fmt.Errorf("kiro: only text is supported in this content")
		}
		text = joinText(text, b.Text)
	}
	return text, nil
}
func joinText(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "\n" + b
}
