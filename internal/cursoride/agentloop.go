package cursoride

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// connectFrameReader reads Connect data frames from a request body. Cursor
// keeps the body open on AgentService/Run while the response streams, then
// sends tool results as more frames on the same body.
type connectFrameReader struct {
	r   io.Reader
	err error
}

// readRemainder is the request body after the last frame read. Used to
// forward a streaming run without a follow-up pump eating the tail.
func (fr *connectFrameReader) readRemainder() []byte {
	if fr == nil || fr.r == nil {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(fr.r, int64(maxCatalogBody)+1))
	return b
}

func newConnectFrameReader(r io.Reader) *connectFrameReader {
	return &connectFrameReader{r: r}
}

func (fr *connectFrameReader) next() ([]byte, error) {
	if fr.err != nil {
		return nil, fr.err
	}
	var hdr [5]byte
	if _, err := io.ReadFull(fr.r, hdr[:]); err != nil {
		fr.err = err
		return nil, err
	}
	flags := hdr[0]
	if flags > 3 {
		fr.err = errf("agent frame flags %d", flags)
		return nil, fr.err
	}
	n := binary.BigEndian.Uint32(hdr[1:5])
	if uint64(n) > uint64(maxCatalogBody) {
		fr.err = errf("agent frame too large")
		return nil, fr.err
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(fr.r, payload); err != nil {
		fr.err = err
		return nil, err
	}
	if flags&0x02 != 0 {
		return fr.next()
	}
	if flags&0x01 != 0 {
		plain, err := gunzipBytes(payload)
		if err != nil {
			fr.err = err
			return nil, err
		}
		payload = plain
	}
	return payload, nil
}

// agentToolResultsFromPayload finds tool-role chat messages in one client
// frame. want lists the call ids still expected this turn.
func agentToolResultsFromPayload(payload []byte, want map[string]bool) []ChatMessage {
	if len(want) == 0 {
		return nil
	}
	var out []ChatMessage
	seen := map[string]bool{}
	add := func(msgs []ChatMessage) {
		for _, m := range msgs {
			if m.Role != "tool" || m.ToolCallID == "" || !want[m.ToolCallID] || seen[m.ToolCallID] {
				continue
			}
			seen[m.ToolCallID] = true
			out = append(out, m)
		}
	}
	if turn, ok, err := readAgentClient(payload); err == nil && ok {
		add(turn.Messages)
	}
	add(scanJSONToolResults(payload, want))
	return out
}

func scanJSONToolResults(payload []byte, want map[string]bool) []ChatMessage {
	var out []ChatMessage
	var walk func([]byte)
	walk = func(b []byte) {
		if looksJSON(b) {
			msgs, err := messagesFromJSON(b)
			if err == nil {
				for _, m := range msgs {
					if m.Role == "tool" && m.ToolCallID != "" && want[m.ToolCallID] {
						out = append(out, m)
					}
				}
			}
		}
		fs, err := parseFields(b)
		if err != nil {
			return
		}
		for _, f := range fs {
			if f.wt == 2 && len(f.b) > 0 {
				walk(f.b)
			}
		}
	}
	walk(payload)
	return out
}

// runAgentLoop streams one composer turn: gateway rounds until the model
// stops without tools, executing each tool call and reading results from
// followups (extra Connect request frames) before turn_ended.
func (p *Proxy) runAgentLoop(ctx context.Context, followups <-chan []byte, turn AgentTurn, route Route, write func([]byte)) {
	chat := Chat{
		Model: route.Model, Effort: route.Effort, Session: turn.ConversationID,
		Messages: append([]ChatMessage(nil), turn.Messages...), Tools: turn.Tools,
		Fast: route.Fast, ContextTokens: route.ContextTokens,
	}
	var execN uint32
	for {
		sealed, done, err := p.agentGatewayRound(ctx, chat, write, &execN)
		if err != nil {
			write(agentTextFrame("cursor ide: " + err.Error()))
			break
		}
		if done {
			break
		}
		if len(sealed) == 0 {
			write(agentTextFrame("cursor ide: tool loop ended without a call"))
			break
		}
		var calls []ToolCall
		want := map[string]bool{}
		for _, t := range sealed {
			calls = append(calls, ToolCall{ID: t.id, Name: t.name, Arguments: t.args})
			want[t.id] = true
		}
		chat.Messages = append(chat.Messages, ChatMessage{Role: "assistant", ToolCalls: calls})

		for len(want) > 0 {
			if followups == nil {
				break
			}
			payload, ok := <-followups
			if !ok {
				Note("agent tool loop eof without tool results")
				break
			}
			for _, m := range agentToolResultsFromPayload(payload, want) {
				chat.Messages = append(chat.Messages, m)
				delete(want, m.ToolCallID)
			}
		}
		if len(want) > 0 {
			break
		}
		Note(fmt.Sprintf("agent tool loop model=%s messages=%d", chat.Model, len(chat.Messages)))
	}
	write(agentTurnEnded())
	write(connectEnd())
}

func drainFollowups(ch <-chan []byte) {
	if ch == nil {
		return
	}
	for range ch {
	}
}

// agentGatewayRound runs one gateway chat completion. When the model ends
// with tool calls, it emits composer tool frames and returns those calls.
// done is true when the model finished without further tools.
func (p *Proxy) agentGatewayRound(ctx context.Context, chat Chat, write func([]byte), execN *uint32) (sealed []toolBuf, done bool, err error) {
	body, err := chat.JSON()
	if err != nil {
		return nil, false, errf("chat")
	}
	base, token := p.Gateway()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, false, errf("gateway")
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range chat.Header() {
		req.Header.Set(k, v)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, false, errf("gateway")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("gateway status %d", resp.StatusCode)
	}
	var asm toolAssembler
	eachPiece(resp.Body, func(piece chatPiece) {
		if piece.thinking != "" {
			write(agentThinkingFrame(piece.thinking))
		}
		if piece.text != "" {
			write(agentTextFrame(piece.text))
		}
		for _, d := range piece.tools {
			if frame := agentPartialFromDelta(&asm, d); frame != nil {
				write(frame)
			}
		}
	})
	sealed = asm.seal()
	if len(sealed) == 0 {
		return nil, true, nil
	}
	for _, t := range sealed {
		*execN++
		frames, ok := agentToolFrames(t.id, t.name, t.args, *execN)
		if !ok {
			write(agentTextFrame("cursor ide: tool arguments are not json"))
			continue
		}
		for _, frame := range frames {
			write(frame)
		}
	}
	// Any sealed call needs follow-up frames (or ends one-shot when none arrive).
	return sealed, false, nil
}
