package cursoride

import (
	"encoding/binary"
	"encoding/json"
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
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
				ToolCalls        []struct {
					Index    *int   `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
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
		p.text += c.Delta.Content
		if c.Delta.ReasoningContent != "" {
			p.thinking += c.Delta.ReasoningContent
		} else {
			p.thinking += c.Delta.Reasoning
		}
		if c.FinishReason != "" {
			p.seal = true
		}
		for i, call := range c.Delta.ToolCalls {
			idx := i
			if call.Index != nil {
				idx = *call.Index
			}
			p.tools = append(p.tools, toolDelta{
				Index: idx, ID: call.ID, Name: call.Function.Name, Args: call.Function.Arguments,
			})
		}
	}
	if p.text == "" && p.thinking == "" && len(p.tools) == 0 && !p.seal {
		return chatPiece{}, false
	}
	return p, true
}
