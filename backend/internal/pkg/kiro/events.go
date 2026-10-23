package kiro

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

const maxResponseBytes = 32 << 20

type Usage struct {
	InputTokens       int      `json:"input_tokens"`
	OutputTokens      int      `json:"output_tokens"`
	InputReported     bool     `json:"input_reported"`
	OutputReported    bool     `json:"output_reported"`
	Credits           *float64 `json:"credits,omitempty"`
	ContextPercentage *float64 `json:"context_percentage,omitempty"`
	Complete          bool     `json:"complete"`
	Source            string   `json:"source"`
}
type Summary struct {
	Response apicompat.AnthropicResponse
	Usage    Usage
}
type toolState struct {
	Index  int
	Name   string
	Input  strings.Builder
	Closed bool
}

// Decode emits structured Anthropic events, reused directly by all downstream
// protocol encoders. It does not serialize/reparse an intermediate SSE stream.
// Summary includes observed usage even when a stream is truncated.
func Decode(r io.Reader, id, model string, inputEstimate int, names map[string]string, emit func(apicompat.AnthropicStreamEvent) error) (summary Summary, err error) {
	summary.Response = apicompat.AnthropicResponse{ID: id, Type: "message", Role: "assistant", Model: model, Content: []apicompat.AnthropicContentBlock{}}
	summary.Usage = Usage{InputTokens: inputEstimate, Source: "estimated"}
	started, terminal := false, false
	contextTrailer, meteringTrailer := false, false
	textIndex, textKind, totalBytes := -1, "", 0
	tools := map[string]*toolState{}
	outputText := strings.Builder{}
	stop := ""
	send := func(e apicompat.AnthropicStreamEvent) error {
		if !started {
			started = true
			message := summary.Response
			message.Content = []apicompat.AnthropicContentBlock{}
			message.Usage.InputTokens = inputEstimate
			if err := emit(apicompat.AnthropicStreamEvent{Type: "message_start", Message: &message}); err != nil {
				return err
			}
		}
		return emit(e)
	}
	closeText := func() error {
		if textIndex < 0 {
			return nil
		}
		i := textIndex
		textIndex = -1
		return send(apicompat.AnthropicStreamEvent{Type: "content_block_stop", Index: &i})
	}
	defer func() {
		if !summary.Usage.OutputReported {
			summary.Usage.OutputTokens = EstimateTokens(outputText.String())
		}
		if summary.Usage.InputReported && summary.Usage.OutputReported {
			summary.Usage.Source = "reported"
		} else if summary.Usage.InputReported || summary.Usage.OutputReported {
			summary.Usage.Source = "mixed"
		}
		summary.Response.Usage = apicompat.AnthropicUsage{InputTokens: summary.Usage.InputTokens, OutputTokens: summary.Usage.OutputTokens}
	}()
	for {
		frame, readErr := ReadFrame(r)
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return summary, readErr
		}
		totalBytes += len(frame.Payload)
		if totalBytes > maxResponseBytes {
			return summary, fmt.Errorf("kiro: response size limit exceeded")
		}
		if typ := frame.Headers[":message-type"]; typ == "exception" || typ == "error" {
			return summary, fmt.Errorf("kiro: upstream stream exception")
		}
		var event map[string]json.RawMessage
		if json.Unmarshal(frame.Payload, &event) != nil {
			return summary, fmt.Errorf("kiro: invalid event payload")
		}
		updateUsage(event, &summary.Usage)
		switch frame.Headers[":event-type"] {
		case "assistantResponseEvent", "reasoningContentEvent":
			if terminal {
				return summary, fmt.Errorf("kiro: content after terminal event")
			}
			kind, key, deltaType := "text", "content", "text_delta"
			if frame.Headers[":event-type"] == "reasoningContentEvent" {
				kind, key, deltaType = "thinking", "text", "thinking_delta"
			}
			var text string
			if json.Unmarshal(event[key], &text) != nil {
				return summary, fmt.Errorf("kiro: invalid text event")
			}
			if text == "" {
				continue
			}
			contextTrailer, meteringTrailer = false, false
			if textIndex < 0 || textKind != kind {
				if err = closeText(); err != nil {
					return summary, err
				}
				textIndex, textKind = len(summary.Response.Content), kind
				block := apicompat.AnthropicContentBlock{Type: kind}
				summary.Response.Content = append(summary.Response.Content, block)
				if err = send(apicompat.AnthropicStreamEvent{Type: "content_block_start", Index: &textIndex, ContentBlock: &block}); err != nil {
					return summary, err
				}
			}
			delta := &apicompat.AnthropicDelta{Type: deltaType}
			if kind == "text" {
				delta.Text = text
				summary.Response.Content[textIndex].Text += text
			} else {
				delta.Thinking = text
				summary.Response.Content[textIndex].Thinking += text
			}
			outputText.WriteString(text)
			if err = send(apicompat.AnthropicStreamEvent{Type: "content_block_delta", Index: &textIndex, Delta: delta}); err != nil {
				return summary, err
			}
		case "toolUseEvent":
			if terminal {
				return summary, fmt.Errorf("kiro: tool after terminal event")
			}
			contextTrailer, meteringTrailer = false, false
			var tid, name string
			_ = json.Unmarshal(event["toolUseId"], &tid)
			_ = json.Unmarshal(event["name"], &name)
			if tid == "" {
				return summary, fmt.Errorf("kiro: tool event lacks ID")
			}
			t := tools[tid]
			if t == nil {
				if name == "" {
					return summary, fmt.Errorf("kiro: tool event lacks name")
				}
				original, ok := names[name]
				if !ok {
					return summary, fmt.Errorf("kiro: upstream called an undeclared tool")
				}
				if err = closeText(); err != nil {
					return summary, err
				}
				t = &toolState{Index: len(summary.Response.Content), Name: name}
				tools[tid] = t
				block := apicompat.AnthropicContentBlock{Type: "tool_use", ID: tid, Name: original, Input: json.RawMessage(`{}`)}
				summary.Response.Content = append(summary.Response.Content, block)
				outputText.WriteString(original)
				if err = send(apicompat.AnthropicStreamEvent{Type: "content_block_start", Index: &t.Index, ContentBlock: &block}); err != nil {
					return summary, err
				}
			}
			if t.Closed || name != "" && name != t.Name {
				return summary, fmt.Errorf("kiro: invalid tool event sequence")
			}
			if raw := event["input"]; len(raw) > 0 && string(raw) != "null" {
				var fragment string
				if json.Unmarshal(raw, &fragment) != nil {
					if !json.Valid(raw) {
						return summary, fmt.Errorf("kiro: invalid tool input")
					}
					fragment = string(raw)
				}
				t.Input.WriteString(fragment)
				outputText.WriteString(fragment)
				if err = send(apicompat.AnthropicStreamEvent{Type: "content_block_delta", Index: &t.Index, Delta: &apicompat.AnthropicDelta{Type: "input_json_delta", PartialJSON: fragment}}); err != nil {
					return summary, err
				}
			}
			var ended bool
			_ = json.Unmarshal(event["stop"], &ended)
			if ended {
				input := t.Input.String()
				if input == "" {
					input = "{}"
				}
				var object map[string]any
				if json.Unmarshal([]byte(input), &object) != nil || object == nil {
					return summary, fmt.Errorf("kiro: incomplete tool JSON")
				}
				summary.Response.Content[t.Index].Input = json.RawMessage(input)
				t.Closed = true
				if err = send(apicompat.AnthropicStreamEvent{Type: "content_block_stop", Index: &t.Index}); err != nil {
					return summary, err
				}
			}
		case "metadataEvent":
			_ = json.Unmarshal(event["stopReason"], &stop)
			if stop == "" {
				_ = json.Unmarshal(event["stop_reason"], &stop)
			}
			terminal = stop != ""
		case "meteringEvent":
			var credits float64
			if json.Unmarshal(event["usage"], &credits) == nil && credits >= 0 {
				meteringTrailer = started
				if summary.Usage.Credits == nil {
					summary.Usage.Credits = new(float64)
				}
				*summary.Usage.Credits += credits
			}
		case "contextUsageEvent":
			var pct float64
			if json.Unmarshal(event["contextUsagePercentage"], &pct) == nil && pct >= 0 && pct <= 100 {
				contextTrailer = started
				summary.Usage.ContextPercentage = &pct
			}
		}
	}
	// The observed IDE stream ends with contextUsageEvent + meteringEvent,
	// followed by a clean HTTP EOF, without metadataEvent/stopReason. Require
	// both valid trailers after the last output, not merely a partial answer.
	// Frame CRC/truncation and transport errors have already returned above.
	if !terminal && started && contextTrailer && meteringTrailer {
		terminal, stop = true, "end_turn"
	}
	if !started || !terminal {
		return summary, fmt.Errorf("kiro: incomplete stream (missing output or stop reason)")
	}
	for _, t := range tools {
		if !t.Closed {
			return summary, fmt.Errorf("kiro: incomplete tool call")
		}
	}
	if err = closeText(); err != nil {
		return summary, err
	}
	switch strings.ToLower(stop) {
	case "end_turn", "stop", "stop_sequence", "complete", "completed":
		stop = "end_turn"
	case "tool_use", "tool_calls":
		stop = "tool_use"
	case "max_tokens", "max_output_tokens", "length":
		stop = "max_tokens"
	default:
		return summary, fmt.Errorf("kiro: unsupported stop reason %q", stop)
	}
	if len(tools) > 0 && stop == "end_turn" {
		stop = "tool_use"
	}
	summary.Response.StopReason = &stop
	summary.Usage.Complete = true
	if !summary.Usage.OutputReported {
		summary.Usage.OutputTokens = EstimateTokens(outputText.String())
	}
	usage := apicompat.AnthropicUsage{InputTokens: summary.Usage.InputTokens, OutputTokens: summary.Usage.OutputTokens}
	if err = send(apicompat.AnthropicStreamEvent{Type: "message_delta", Delta: &apicompat.AnthropicDelta{StopReason: stop}, Usage: &usage}); err != nil {
		return summary, err
	}
	err = send(apicompat.AnthropicStreamEvent{Type: "message_stop"})
	return summary, err
}

func updateUsage(event map[string]json.RawMessage, usage *Usage) {
	for _, entry := range []struct {
		Keys     []string
		Value    *int
		Reported *bool
	}{
		{[]string{"inputTokens", "input_tokens", "promptTokens"}, &usage.InputTokens, &usage.InputReported},
		{[]string{"outputTokens", "output_tokens", "completionTokens"}, &usage.OutputTokens, &usage.OutputReported},
	} {
		for _, key := range entry.Keys {
			var n int
			if raw, ok := event[key]; ok && json.Unmarshal(raw, &n) == nil && n >= 0 {
				*entry.Value = n
				*entry.Reported = true
				break
			}
		}
	}
	for _, key := range []string{"usage", "tokenUsage", "metadata", "metrics"} {
		var nested map[string]json.RawMessage
		if raw := event[key]; len(raw) > 0 && raw[0] == '{' && json.Unmarshal(raw, &nested) == nil {
			updateUsage(nested, usage)
		}
	}
}

// EstimateTokens is a disclosed fallback: ASCII chars/4 + non-ASCII runes.
// Neither credits nor context percentages participate in this estimate.
func EstimateTokens(text string) int {
	ascii, other := 0, 0
	for _, r := range text {
		if r < 128 {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}
