package cursoride

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// connectFrame is a Connect data frame: flag 0 and a big-endian length.
func connectFrame(msg []byte) []byte {
	out := make([]byte, 5+len(msg))
	binary.BigEndian.PutUint32(out[1:5], uint32(len(msg)))
	copy(out[5:], msg)
	return out
}

// connectEnd is the Connect end-of-stream frame. Cursor's stream stays
// open until this, after the run's last invocation.
func connectEnd() []byte {
	payload := []byte("{}")
	out := make([]byte, 5+len(payload))
	out[0] = 0x02
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

// knownEffort is an effort Cursor's prompt metadata accepts. Anything else
// is left unset: a value it does not know is not sent as that value.
func knownEffort(s string) bool {
	switch s {
	case "low", "medium", "high", "extra-high", "xhigh", "max":
		return true
	default:
		return false
	}
}

// The mode ids Cursor's local loop looks for on run_ready. Only agent
// enables the ask-question tool, which is what Cursor's own fallback does.
var promptModes = []struct {
	id  string
	ask bool
}{
	{"agent", true},
	{"chat", false},
	{"plan", false},
	{"debug", false},
	{"triage", false},
	{"multitask", false},
}

// runReady is RunInferenceServerMessage.run_ready. vendor and
// prompt_version are the client's own fallback for a model it has no
// family for ("unknown", "latest"), so the loop builds the invoke.
func runReady(modelID, effort string, context int) []byte {
	var model wbuf
	model.str(1, modelID)
	var meta wbuf
	meta.str(1, "unknown")
	meta.str(2, "latest")
	if knownEffort(effort) {
		meta.str(30, effort)
	}
	if context > 0 {
		meta.u32(32, uint32(context))
	}
	for _, m := range promptModes {
		var one wbuf
		one.str(1, m.id)
		if m.ask {
			one.bool(2, true)
		}
		meta.msg(46, []byte(one))
	}
	var ready wbuf
	ready.msg(1, []byte(model))
	ready.msg(4, []byte(meta))
	var server wbuf
	server.msg(2, []byte(ready))
	return connectFrame([]byte(server))
}

// invocationIdFrame is InferenceStreamResponse.invocation_id. Cursor's
// local loop resolves O.invocationId from this arm; without it it invents
// a random UUID and then logs Invocation ID mismatch / empty response.
func invocationIdFrame(invocation string) []byte {
	var info wbuf
	info.str(1, invocation)
	var stream wbuf
	stream.msg(7, []byte(info))
	return invocationFrame(invocation, []byte(stream))
}

// textFrame is InferenceStreamResponse.text_part. When final is true Cursor
// only emits a stream finish and ignores text, so callers put content on
// final=false frames and send one empty final=true frame afterward.
func textFrame(invocation, text string, final bool) []byte {
	var part wbuf
	part.str(1, text)
	if final {
		part.bool(2, true)
	}
	var stream wbuf
	stream.msg(1, []byte(part))
	return invocationFrame(invocation, []byte(stream))
}

func thinkingFrame(invocation, text string) []byte {
	var part wbuf
	part.str(1, text)
	var stream wbuf
	stream.msg(9, []byte(part))
	return invocationFrame(invocation, []byte(stream))
}

func invocationFrame(invocation string, stream []byte) []byte {
	var inv wbuf
	inv.str(1, invocation)
	inv.msg(2, stream)
	var server wbuf
	server.msg(3, []byte(inv))
	return connectFrame([]byte(server))
}

func endFrame(invocation, message string) []byte {
	var end wbuf
	end.str(1, invocation)
	if message != "" {
		var er wbuf
		er.u32(1, 13) // internal
		er.str(2, message)
		end.msg(2, []byte(er))
	}
	var server wbuf
	server.msg(4, []byte(end))
	return connectFrame([]byte(server))
}

// chatPiece is one delta of an OpenAI chat stream.
type chatPiece struct {
	text, thinking string
	tools          []toolDelta
	seal           bool
	done           bool
}

func chatPieces(line string) (chatPiece, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "data:") {
		return chatPiece{}, false
	}
	data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if data == "[DONE]" {
		return chatPiece{done: true}, true
	}
	var body struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Delta        struct {
				Content          json.RawMessage   `json:"content"`
				ReasoningContent json.RawMessage   `json:"reasoning_content"`
				Reasoning        json.RawMessage   `json:"reasoning"`
				ToolCalls        []json.RawMessage `json:"tool_calls"`
			} `json:"delta"`
			Message struct {
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(data), &body) != nil {
		return chatPiece{}, false
	}
	if body.Error.Message != "" {
		return chatPiece{}, false
	}
	var p chatPiece
	for _, c := range body.Choices {
		p.text += jsonString(c.Delta.Content)
		if think := jsonString(c.Delta.ReasoningContent); think != "" {
			p.thinking += think
		} else {
			p.thinking += jsonString(c.Delta.Reasoning)
		}
		if c.FinishReason != "" {
			p.seal = true
		}
		calls := c.Delta.ToolCalls
		if len(calls) == 0 {
			calls = c.Message.ToolCalls
		}
		for i, raw := range calls {
			d, ok := parseToolDelta(raw, i)
			if !ok {
				continue
			}
			p.tools = append(p.tools, d)
		}
	}
	if p.text == "" && p.thinking == "" && len(p.tools) == 0 && !p.seal {
		return chatPiece{}, false
	}
	return p, true
}

// parseToolDelta reads one streamed tool call. index may be a float
// (0.0), and arguments may be a JSON object rather than a string. Either
// one used to fail the whole SSE line, so the sealed call kept the name
// and lost the strings Cursor validates.
func parseToolDelta(raw json.RawMessage, fallback int) (toolDelta, bool) {
	var call struct {
		Index     json.RawMessage `json:"index"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Input     json.RawMessage `json:"input"`
		Function  struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Input     json.RawMessage `json:"input"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &call) != nil {
		return toolDelta{}, false
	}
	idx, ok := indexOf(call.Index, fallback)
	if !ok {
		return toolDelta{}, false
	}
	name := call.Function.Name
	if name == "" {
		name = call.Name
	}
	args := firstArgText(call.Function.Arguments, call.Arguments, call.Function.Input, call.Input)
	if name == "" && call.ID == "" && args == "" {
		return toolDelta{}, false
	}
	return toolDelta{Index: idx, ID: call.ID, Name: name, Args: args}, true
}

// indexOf reports the tool-call index. A missing index uses fallback.
// A present index that is not a non-negative integer is rejected: 1.5
// must not be truncated onto another call.
func indexOf(raw json.RawMessage, fallback int) (int, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return fallback, true
	}
	var n int
	if json.Unmarshal(raw, &n) == nil && n >= 0 {
		return n, true
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil && f >= 0 && f <= math.MaxInt32 && f == math.Trunc(f) {
		return int(f), true
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		n, err := strconv.Atoi(strings.TrimSpace(text))
		if err == nil && n >= 0 {
			return n, true
		}
	}
	return 0, false
}

func firstArgText(raws ...json.RawMessage) string {
	hold := ""
	for _, raw := range raws {
		text, ok := argText(raw)
		if !ok || strings.TrimSpace(text) == "" {
			continue
		}
		// "{}" beside a sibling input object is the placeholder, not the
		// call. An empty string was already skipped; this is the same case
		// when the relay wrote the empty object out.
		if emptyJSONObject(text) {
			if hold == "" {
				hold = strings.TrimSpace(text)
			}
			continue
		}
		return text
	}
	return hold
}

func argText(raw json.RawMessage) (string, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return "", false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, true
	}
	if json.Valid(raw) {
		return s, true
	}
	return "", false
}

func jsonString(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return ""
	}
	return text
}
