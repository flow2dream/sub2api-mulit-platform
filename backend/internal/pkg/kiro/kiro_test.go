package kiro

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

// Fixtures are synthetic AWS EventStream frames, not recordings from an account.
func fixtureFrame(event, payload string) []byte {
	var headers bytes.Buffer
	for _, h := range [][2]string{{":message-type", "event"}, {":event-type", event}} {
		headers.WriteByte(byte(len(h[0])))
		headers.WriteString(h[0])
		headers.WriteByte(7)
		_ = binary.Write(&headers, binary.BigEndian, uint16(len(h[1])))
		headers.WriteString(h[1])
	}
	var frame bytes.Buffer
	_ = binary.Write(&frame, binary.BigEndian, uint32(16+headers.Len()+len(payload)))
	_ = binary.Write(&frame, binary.BigEndian, uint32(headers.Len()))
	_ = binary.Write(&frame, binary.BigEndian, crc32.ChecksumIEEE(frame.Bytes()))
	frame.Write(headers.Bytes())
	frame.WriteString(payload)
	_ = binary.Write(&frame, binary.BigEndian, crc32.ChecksumIEEE(frame.Bytes()))
	return frame.Bytes()
}
func fixtureStream(events ...[2]string) []byte {
	var out []byte
	for _, e := range events {
		out = append(out, fixtureFrame(e[0], e[1])...)
	}
	return out
}

type fragmentedReader struct{ io.Reader }

func (r fragmentedReader) Read(b []byte) (int, error) {
	if len(b) > 1 {
		b = b[:1]
	}
	return r.Reader.Read(b)
}

func TestFrameValidationAndPacketBoundaries(t *testing.T) {
	good := fixtureFrame("assistantResponseEvent", `{"content":"你好🌏"}`)
	frame, err := ReadFrame(fragmentedReader{bytes.NewReader(good)})
	if err != nil || !strings.Contains(string(frame.Payload), "你好") {
		t.Fatalf("fragmented frame: %v", err)
	}
	for _, part := range []int{1, 7, 11, len(good) - 1} {
		if _, err = ReadFrame(bytes.NewReader(good[:part])); err == nil {
			t.Fatalf("accepted truncated frame %d", part)
		}
	}
	for _, position := range []int{0, 8, 15, len(good) - 1} {
		broken := append([]byte(nil), good...)
		broken[position] ^= 0xff
		if _, err = ReadFrame(bytes.NewReader(broken)); err == nil {
			t.Fatalf("accepted corrupt byte %d", position)
		}
	}
	var prelude [12]byte
	binary.BigEndian.PutUint32(prelude[:4], MaxFrameSize+1)
	binary.BigEndian.PutUint32(prelude[8:], crc32.ChecksumIEEE(prelude[:8]))
	if _, err = ReadFrame(bytes.NewReader(prelude[:])); err == nil {
		t.Fatal("accepted oversized allocation")
	}
	for _, header := range [][]byte{{1}, {1, 'a', 7, 0, 4, 'x'}, {1, 'a', 99}} {
		if _, err = decodeHeaders(header); err == nil {
			t.Fatal("accepted invalid header")
		}
	}
}

func TestDecodeParallelToolsAndNativeMetering(t *testing.T) {
	raw := fixtureStream(
		[2]string{"assistantResponseEvent", `{"content":"查一下"}`},
		[2]string{"toolUseEvent", `{"toolUseId":"a","name":"read","input":"{\"path\":"}`},
		[2]string{"toolUseEvent", `{"toolUseId":"b","name":"read","input":"{\"path\":\"b\"}","stop":true}`},
		[2]string{"toolUseEvent", `{"toolUseId":"a","input":"\"a\"}","stop":true}`},
		[2]string{"meteringEvent", `{"usage":0.25}`},
		[2]string{"contextUsageEvent", `{"contextUsagePercentage":51}`},
		[2]string{"metadataEvent", `{"stopReason":"tool_use","usage":{"inputTokens":123,"outputTokens":45}}`},
	)
	var events []apicompat.AnthropicStreamEvent
	summary, err := Decode(fragmentedReader{bytes.NewReader(raw)}, "msg_test", "model", 999, map[string]string{"read": "read"}, func(e apicompat.AnthropicStreamEvent) error { events = append(events, e); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Response.Content) != 3 || string(summary.Response.Content[1].Input) != `{"path":"a"}` || string(summary.Response.Content[2].Input) != `{"path":"b"}` {
		t.Fatalf("crossed tool arguments: %+v", summary.Response.Content)
	}
	if summary.Usage.Source != "reported" || summary.Usage.InputTokens != 123 || summary.Usage.OutputTokens != 45 || *summary.Usage.Credits != 0.25 || *summary.Usage.ContextPercentage != 51 || !summary.Usage.Complete {
		t.Fatalf("bad usage: %+v", summary.Usage)
	}
	if events[len(events)-1].Type != "message_stop" {
		t.Fatal("missing completion")
	}
}

func TestDecodeDoesNotCompleteTruncatedOrInvalidTools(t *testing.T) {
	for _, raw := range [][]byte{
		fixtureStream([2]string{"assistantResponseEvent", `{"content":"partial"}`}),
		fixtureStream([2]string{"toolUseEvent", `{"toolUseId":"a","name":"read","input":"{"}`}, [2]string{"metadataEvent", `{"stopReason":"tool_use"}`}),
		fixtureStream([2]string{"toolUseEvent", `{"toolUseId":"a","name":"read","input":"{","stop":true}`}),
		fixtureStream([2]string{"assistantResponseEvent", `{"content":"partial"}`}, [2]string{"metadataEvent", `{"stopReason":"unknown"}`}),
	} {
		completed := false
		summary, err := Decode(bytes.NewReader(raw), "id", "m", 10, map[string]string{"read": "read"}, func(e apicompat.AnthropicStreamEvent) error {
			completed = completed || e.Type == "message_stop"
			return nil
		})
		if err == nil || completed || summary.Usage.Complete {
			t.Fatalf("false success: %v %+v", err, summary)
		}
	}
}

func TestEstimatedTokensNeverUseCreditsOrContextPercent(t *testing.T) {
	raw := fixtureStream([2]string{"assistantResponseEvent", `{"content":"abcdefgh"}`}, [2]string{"meteringEvent", `{"usage":900}`}, [2]string{"contextUsageEvent", `{"contextUsagePercentage":90}`}, [2]string{"metadataEvent", `{"stopReason":"end_turn"}`})
	summary, err := Decode(bytes.NewReader(raw), "id", "m", 7, nil, func(apicompat.AnthropicStreamEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if summary.Usage.Source != "estimated" || summary.Usage.InputTokens != 7 || summary.Usage.OutputTokens != 2 {
		t.Fatalf("bad estimation: %+v", summary.Usage)
	}
}

func TestBuildRequestHistoryToolsAndIsolation(t *testing.T) {
	raw := `{"model":"test-model","system":"system rules","max_tokens":128,"tools":[{"name":"ns.read","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"ns.read","input":{"path":"a"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"file text"}]}]}`
	var req apicompat.AnthropicRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(req)
	first, err := BuildRequest(&req, Credentials{EndpointFamily: "q", ProfileARN: "profile"}, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildRequest(&req, Credentials{EndpointFamily: "runtime", AuthMethod: "api_key"}, "session-2")
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(req)
	if !bytes.Equal(before, after) {
		t.Fatal("mutated original request")
	}
	if first.ProfileARN != "profile" || second.ProfileARN != "" || second.ConversationState.CurrentMessage.User.Origin != "KIRO_CLI" || !strings.HasPrefix(first.ConversationState.History[0].User.Content, "system rules") {
		t.Fatal("account fields leaked")
	}
	tool := first.ConversationState.CurrentMessage.User.Context.Tools[0].Specification.Name
	if first.ToolNames[tool] != "ns.read" || first.ConversationState.History[1].Assistant.ToolUses[0].Name != tool || first.ConversationState.CurrentMessage.User.Context.ToolResults[0].ID != "a" {
		t.Fatal("tool mapping not reversible")
	}
	req.Messages[2].Content = json.RawMessage(`[{"type":"image","source":{"type":"base64","data":"AA=="}}]`)
	if _, err = BuildRequest(&req, Credentials{}, "s"); err == nil {
		t.Fatal("silently dropped unsupported image")
	}
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }
func TestCredentialsRegionalRefreshAndRotation(t *testing.T) {
	raw := map[string]any{"refreshToken": "old", "clientId": "id", "clientSecret": "secret", "authRegion": "eu-west-1", "apiRegion": "us-east-1", "expiresAt": "2026-10-22T01:00:00Z", "model_mapping": map[string]any{"m": "upstream"}}
	c, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, rotated := range []string{"", "new"} {
		updated, err := Refresh(context.Background(), doerFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host != "oidc.eu-west-1.amazonaws.com" {
				t.Fatal("used inference region for OIDC")
			}
			var body map[string]string
			_ = json.NewDecoder(req.Body).Decode(&body)
			if body["clientSecret"] != "secret" || body["grantType"] != "refresh_token" {
				t.Fatal("bad refresh payload")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"accessToken":"access","expiresIn":3600,"refreshToken":"` + rotated + `"}`))}, nil
		}), c, raw)
		if err != nil {
			t.Fatal(err)
		}
		expected := "old"
		if rotated != "" {
			expected = rotated
		}
		if updated["refresh_token"] != expected || updated["model_mapping"] == nil || updated["client_secret"] != "secret" {
			t.Fatal("lost refreshed or unrelated credentials")
		}
		if _, ok := updated["refreshToken"]; ok {
			t.Fatal("retained stale alias")
		}
	}
	if raw["refreshToken"] != "old" {
		t.Fatal("mutated credential snapshot")
	}
	key, err := Parse(map[string]any{"api_key": "native-key"})
	if err != nil {
		t.Fatal(err)
	}
	payload := &Payload{}
	req, err := NewInferenceRequest(context.Background(), key, payload)
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.Host != "runtime.us-east-1.kiro.dev" || req.Header.Get("tokentype") != "API_KEY" || req.Header.Get("X-Amz-Target") == "" {
		t.Fatal("API key routed as generic OpenAI")
	}
}
