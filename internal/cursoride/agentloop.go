package cursoride

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
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
func agentToolResultsFromPayload(payload []byte, want map[string]bool, execOf map[uint32]string) []ChatMessage {
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
	add(execToolResults(payload, want, execOf))
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
	execOf := map[uint32]string{}
	for {
		before := execN
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
		for i, t := range sealed {
			calls = append(calls, ToolCall{ID: t.id, Name: t.name, Arguments: t.args})
			want[t.id] = true
			// agentGatewayRound numbers each exec from before+1, in order.
			execOf[before+uint32(i)+1] = t.id
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
			got := agentToolResultsFromPayload(payload, want, execOf)
			if len(got) == 0 {
				noteUnparsedToolFrame(payload)
			}
			for _, m := range got {
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
	noteRequestToolArgs(chat.Messages)
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
	var said strings.Builder
	eachPiece(resp.Body, func(piece chatPiece) {
		if piece.thinking != "" {
			write(agentThinkingFrame(piece.thinking))
			said.WriteString(piece.thinking)
			said.WriteByte('\n')
		}
		if piece.text != "" {
			write(agentTextFrame(piece.text))
			said.WriteString(piece.text)
			said.WriteByte('\n')
		}
		for _, d := range piece.tools {
			if frame := agentPartialFromDelta(&asm, d); frame != nil {
				write(frame)
			}
		}
	})
	text := said.String()
	run := sealRound(&asm, text, func(msg string) { write(agentTextFrame(msg)) })
	if len(run) == 0 {
		return nil, true, nil
	}
	markers := markupMarkers(text)
	for _, t := range run {
		*execN++
		frames, ok := agentToolFrames(t.id, t.name, t.args, *execN)
		frameState := "skipped"
		if !ok {
			write(agentTextFrame("cursor ide: tool arguments are not json"))
		} else {
			frameState = "written"
			for _, frame := range frames {
				write(frame)
			}
		}
		noteToolTrace("frames", t.name, t.args, t.frags, t.placeholders, t.fill, markers, frameState)
	}
	// Any run call needs follow-up frames (or ends one-shot when none arrive).
	return run, false, nil
}

// sealRound closes asm once the gateway body has ended. A native call
// whose arguments stayed "{}" is filled from text the gateway left in
// the same reply. run is the calls that have the strings Pi validates.
// where=seal, where=frames exec=skipped, and where=round are logged
// here, on whichever entry read the body. The caller logs exec=written
// after it puts the call on the wire.
func sealRound(asm *toolAssembler, text string, say func(string)) []toolBuf {
	sealed := asm.seal()
	fillEmptyToolArgs(sealed, text)
	markers := markupMarkers(text)
	var run []toolBuf
	for _, t := range sealed {
		ready := piCallReady(t.name, t.args)
		state := "skip"
		if ready {
			state = "run"
			run = append(run, t)
		} else if strings.TrimSpace(t.args) != "" && !json.Valid([]byte(t.args)) && say != nil {
			say("cursor ide: tool arguments are not json")
		}
		noteToolTrace("seal", t.name, t.args, t.frags, t.placeholders, t.fill, markers, state)
		if !ready {
			noteToolTrace("frames", t.name, t.args, t.frags, t.placeholders, t.fill, markers, "skipped")
		}
	}
	noteRound(len(sealed), len(run))
	return run
}

// noteRequestToolArgs records whether this gateway body is about to send
// assistant tool calls whose arguments are still {}. One summary line
// always, plus one line per empty call. Argument values are not logged.
func noteRequestToolArgs(msgs []ChatMessage) {
	var total, empty, logged int
	for _, m := range msgs {
		if m.Role != "assistant" {
			continue
		}
		for _, c := range m.ToolCalls {
			total++
			if !callArgsEmpty(c.Arguments) {
				continue
			}
			empty++
			// A long replay of empty calls would otherwise fill the
			// diagnostic file before this round's seal line. The summary
			// still counts every one.
			if logged < 4 {
				noteToolTrace("request", c.Name, c.Arguments, 0, 0, "", "-", "na")
				logged++
			}
		}
	}
	if total == 0 {
		return
	}
	Note(fmt.Sprintf("agent tool trace where=request calls=%d empty=%d", total, empty))
}
