package kiro

import (
	"bytes"
	"io"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

// Synthetic frames matching the sequence observed from MiniMax M2.5. No
// credential, conversation identifier or captured account data is retained.
func ideCompletedStream() []byte {
	return fixtureStream(
		[2]string{"assistantResponseEvent", `{"content":"","modelId":"minimax-m2.5"}`},
		[2]string{"reasoningContentEvent", `{"text":"Answer briefly."}`},
		[2]string{"assistantResponseEvent", `{"content":"OK","modelId":"minimax-m2.5"}`},
		[2]string{"contextUsageEvent", `{"contextUsagePercentage":1.8}`},
		[2]string{"meteringEvent", `{"usage":0.02,"unit":"credit"}`},
	)
}

func TestIDEStreamCompletesWithValidatedTrailers(t *testing.T) {
	var events []string
	s, err := Decode(fragmentedReader{bytes.NewReader(ideCompletedStream())}, "msg", "minimax-m2.5", 5, nil, func(e apicompat.AnthropicStreamEvent) error {
		events = append(events, e.Type)
		return nil
	})
	require.NoError(t, err)
	require.True(t, s.Usage.Complete)
	require.Equal(t, "end_turn", *s.Response.StopReason)
	require.Equal(t, "OK", s.Response.Content[1].Text)
	require.Equal(t, 0.02, *s.Usage.Credits)
	require.Equal(t, "message_stop", events[len(events)-1])
}

type ideFailAtEndReader struct{ *bytes.Reader }

func (r ideFailAtEndReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func TestIDEStreamDoesNotHideTruncation(t *testing.T) {
	full := ideCompletedStream()
	text := [2]string{"assistantResponseEvent", `{"content":"partial"}`}
	context := [2]string{"contextUsageEvent", `{"contextUsagePercentage":1}`}
	meter := [2]string{"meteringEvent", `{"usage":0.02}`}
	corrupt := append([]byte{}, full...)
	corrupt[len(corrupt)-1] ^= 1
	cases := map[string]io.Reader{
		"missing trailers":          bytes.NewReader(fixtureStream(text)),
		"missing metering":          bytes.NewReader(fixtureStream(text, context)),
		"missing context":           bytes.NewReader(fixtureStream(text, meter)),
		"no output":                 bytes.NewReader(fixtureStream(context, meter)),
		"invalid context":           bytes.NewReader(fixtureStream(text, [2]string{"contextUsageEvent", `{"contextUsagePercentage":101}`}, meter)),
		"invalid metering":          bytes.NewReader(fixtureStream(text, context, [2]string{"meteringEvent", `{"usage":-1}`})),
		"content after trailers":    bytes.NewReader(append(append([]byte{}, full...), fixtureFrame(text[0], text[1])...)),
		"partial trailer":           bytes.NewReader(full[:len(full)-1]),
		"bad crc":                   bytes.NewReader(corrupt),
		"prelude without frame":     bytes.NewReader(append(append([]byte{}, full...), fixtureFrame(text[0], text[1])[:12]...)),
		"transport failure":         ideFailAtEndReader{bytes.NewReader(full)},
		"unclosed tool":             bytes.NewReader(fixtureStream([2]string{"toolUseEvent", `{"toolUseId":"a","name":"read","input":"{}"}`}, context, meter)),
		"unsupported explicit stop": bytes.NewReader(append(append([]byte{}, full...), fixtureFrame("metadataEvent", `{"stopReason":"unknown"}`)...)),
	}
	for name, reader := range cases {
		t.Run(name, func(t *testing.T) {
			stopped := false
			s, err := Decode(reader, "id", "minimax-m2.5", 0, map[string]string{"read": "read"}, func(e apicompat.AnthropicStreamEvent) error {
				stopped = stopped || e.Type == "message_stop"
				return nil
			})
			require.Error(t, err)
			require.False(t, stopped)
			require.False(t, s.Usage.Complete)
		})
	}
}

func TestIDEStreamClosedToolUsesToolStopReason(t *testing.T) {
	raw := fixtureStream(
		[2]string{"toolUseEvent", `{"toolUseId":"a","name":"read","input":"{}","stop":true}`},
		[2]string{"contextUsageEvent", `{"contextUsagePercentage":1}`},
		[2]string{"meteringEvent", `{"usage":0.02}`},
	)
	s, err := Decode(bytes.NewReader(raw), "id", "model", 0, map[string]string{"read": "read"}, func(apicompat.AnthropicStreamEvent) error { return nil })
	require.NoError(t, err)
	require.Equal(t, "tool_use", *s.Response.StopReason)
}
