package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// The general compatibility converters intentionally repair/drop some unknown
// content. Validate before conversion so Kiro never silently loses that input.
func kiroTextContent(value gjson.Result) error {
	if !value.Exists() || value.Type == gjson.Null || value.Type == gjson.String {
		return nil
	}
	if !value.IsArray() {
		return fmt.Errorf("kiro: content must be text or text blocks")
	}
	for _, part := range value.Array() {
		switch part.Get("type").String() {
		case "text", "input_text", "output_text":
			if part.Get("text").Type != gjson.String {
				return fmt.Errorf("kiro: text block requires text")
			}
		default:
			return fmt.Errorf("kiro: non-text content is unsupported")
		}
	}
	return nil
}

func validateKiroToolPairs(calls, outputs map[string]bool) error {
	for id := range calls {
		if !outputs[id] {
			return fmt.Errorf("kiro: historical tool calls require their results")
		}
	}
	for id := range outputs {
		if !calls[id] {
			return fmt.Errorf("kiro: tool result has no corresponding call")
		}
	}
	return nil
}

func prepareKiroResponsesInput(body []byte) ([]byte, error) {
	input := gjson.GetBytes(body, "input")
	if input.Type == gjson.String {
		return body, nil
	}
	if !input.IsArray() {
		return nil, fmt.Errorf("kiro: input must be text or complete history")
	}
	calls, outputs := map[string]bool{}, map[string]bool{}
	items := make([]json.RawMessage, 0, len(input.Array()))
	for _, item := range input.Array() {
		typ := item.Get("type").String()
		switch typ {
		case "", "message":
			switch item.Get("role").String() {
			case "user", "assistant", "system", "developer":
			default:
				return nil, fmt.Errorf("kiro: unsupported message role")
			}
			if err := kiroTextContent(item.Get("content")); err != nil {
				return nil, err
			}
		case "function_call":
			id := item.Get("call_id").String()
			args := item.Get("arguments").String()
			if id == "" || calls[id] || item.Get("name").String() == "" || !json.Valid([]byte(args)) || !gjson.Parse(args).IsObject() {
				return nil, fmt.Errorf("kiro: invalid historical function call")
			}
			calls[id] = true
		case "function_call_output":
			id := item.Get("call_id").String()
			if id == "" || outputs[id] {
				return nil, fmt.Errorf("kiro: invalid historical tool result")
			}
			outputs[id] = true
			if err := kiroTextContent(item.Get("output")); err != nil {
				return nil, err
			}
		case "reasoning":
			// Native history has no encrypted reasoning field. Retain visible
			// summaries; reject ciphertext-only history rather than discard it.
			var parts []string
			for _, part := range item.Get("summary").Array() {
				if text := part.Get("text").String(); text != "" {
					parts = append(parts, text)
				}
			}
			for _, part := range item.Get("content").Array() {
				if text := part.Get("text").String(); text != "" {
					parts = append(parts, text)
				}
			}
			if len(parts) == 0 {
				return nil, fmt.Errorf("kiro: reasoning history requires visible text; encrypted-only history is unsupported")
			}
			raw, _ := json.Marshal(map[string]any{"type": "message", "role": "assistant", "content": strings.Join(parts, "\n")})
			items = append(items, raw)
			continue
		default:
			return nil, fmt.Errorf("kiro: unsupported Responses history item")
		}
		items = append(items, json.RawMessage(item.Raw))
	}
	if err := validateKiroToolPairs(calls, outputs); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(body, "input", raw)
}

func validateKiroChatInput(body []byte) error {
	calls, outputs := map[string]bool{}, map[string]bool{}
	legacy := false
	for _, message := range gjson.GetBytes(body, "messages").Array() {
		role := message.Get("role").String()
		switch role {
		case "user", "assistant", "system", "developer", "tool", "function":
		default:
			return fmt.Errorf("kiro: unsupported chat role")
		}
		if err := kiroTextContent(message.Get("content")); err != nil {
			return err
		}
		if message.Get("function_call").Exists() || role == "function" {
			legacy = true
		}
		for _, tool := range message.Get("tool_calls").Array() {
			id := tool.Get("id").String()
			args := tool.Get("function.arguments").String()
			if id == "" || calls[id] || tool.Get("type").String() != "function" || tool.Get("function.name").String() == "" || !json.Valid([]byte(args)) || !gjson.Parse(args).IsObject() {
				return fmt.Errorf("kiro: invalid chat tool call")
			}
			calls[id] = true
		}
		if role == "tool" {
			id := message.Get("tool_call_id").String()
			if id == "" || outputs[id] {
				return fmt.Errorf("kiro: invalid chat tool result")
			}
			outputs[id] = true
		}
	}
	if legacy {
		return fmt.Errorf("kiro: legacy function_call history is unsupported; use tool_calls")
	}
	return validateKiroToolPairs(calls, outputs)
}
