package cursoride

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"strings"
	"testing"
)

func agentCatalog() []Model {
	return []Model{
		{ID: "aicodings-openai/gpt-6-astra", Name: "GPT-6 Astra", Reasoning: true, Efforts: []string{"low", "medium", "high"}},
		{ID: "codex/gpt-5.6-luna", Name: "GPT-5.6 Luna"},
		{ID: "workbuddy/deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash"},
	}
}

func TestAgentRunRoutesPrefixedCatalogIDs(t *testing.T) {
	models := agentCatalog()
	cases := []struct {
		id, effort, text string
		params           []Parameter
	}{
		{id: "aicodings-openai/gpt-6-astra", text: "你好"},
		{id: "codex/gpt-5.6-luna", text: "hello"},
		{id: "workbuddy/deepseek-v4.1-flash", text: "hi"},
		{
			id:     "aicodings-openai/gpt-6-astra[context=200k,reasoning=high,fast=false]",
			effort: "high", text: "你好",
		},
		{
			id: "aicodings-openai/gpt-6-astra", effort: "high", text: "你好",
			params: []Parameter{{ID: "reasoning", Value: "high"}},
		},
	}
	for _, tc := range cases {
		body := agentRunBody(tc.id, "conv-1", tc.text, tc.params)
		turn, ok, err := ReadAgentRun(body)
		if err != nil || !ok {
			t.Fatalf("ReadAgentRun(%q) not a run", tc.id)
		}
		if turn.Text != tc.text || turn.ConversationID != "conv-1" {
			t.Fatalf("turn %+v", turn)
		}
		route, ours, err := RouteModel(models, turn.ModelID, turn.Params)
		if err != nil || !ours {
			t.Fatalf("%s ours=%v err=%v", tc.id, ours, err)
		}
		wantID := tc.id
		if i := strings.IndexByte(wantID, '['); i > 0 {
			wantID = wantID[:i]
		}
		if route.Model != wantID || route.Effort != tc.effort {
			t.Fatalf("%s route %+v, want model %s effort %q", tc.id, route, wantID, tc.effort)
		}
	}
}

func TestAgentRunModelDetailsFallsBackWhenRequestedModelIsAbsent(t *testing.T) {
	body := frameOf(agentClient(agentRunDetailsOnly("codex/gpt-5.6-luna", "conv-9", "你好")))
	turn, ok, err := ReadAgentRun(body)
	if err != nil || !ok || turn.ModelID != "codex/gpt-5.6-luna" || turn.Text != "你好" {
		t.Fatalf("turn ok=%v %+v", ok, turn)
	}
}

func TestAgentRunGzipFrameAndCursorModelStayDistinct(t *testing.T) {
	plain := agentClient(agentRunMessage("workbuddy/deepseek-v4.1-flash", "c", "你好", nil))
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 5+buf.Len())
	frame[0] = 0x01
	binary.BigEndian.PutUint32(frame[1:5], uint32(buf.Len()))
	copy(frame[5:], buf.Bytes())
	turn, ok, err := ReadAgentRun(frame)
	if err != nil || !ok || turn.ModelID != "workbuddy/deepseek-v4.1-flash" {
		t.Fatalf("gzip turn ok=%v %+v", ok, turn)
	}
	own := frameOf(agentClient(agentRunMessage("grok-4.7", "c", "hi", nil)))
	got, ok, err := ReadAgentRun(own)
	if err != nil || !ok || got.ModelID != "grok-4.7" {
		t.Fatalf("cursor model ok=%v %+v", ok, got)
	}
	if _, ok, err := ReadAgentRun([]byte{0x0a, 0x00}); err != nil || ok {
		t.Fatal("empty client message was a run")
	}
}

func TestAgentFramesCarryTextThinkingAndTurnEnd(t *testing.T) {
	text := connectPayloadsMust(t, agentTextFrame("你好"))
	fs, err := parseFields(text)
	if err != nil {
		t.Fatal(err)
	}
	upd, err := parseFields(fieldBytes(fs, 1)[0])
	if err != nil {
		t.Fatal(err)
	}
	delta, err := parseFields(fieldBytes(upd, 1)[0])
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := fieldString(delta, 1); got != "你好" {
		t.Fatalf("text = %q", got)
	}
	think := connectPayloadsMust(t, agentThinkingFrame("ponder"))
	tfs, err := parseFields(think)
	if err != nil {
		t.Fatal(err)
	}
	tupd, err := parseFields(fieldBytes(tfs, 1)[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fieldBytesOK(tupd, 4); !ok {
		t.Fatal("thinking delta missing")
	}
	end := connectPayloadsMust(t, agentTurnEnded())
	efs, err := parseFields(end)
	if err != nil {
		t.Fatal(err)
	}
	eupd, err := parseFields(fieldBytes(efs, 1)[0])
	if err != nil {
		t.Fatal(err)
	}
	if !hasField(eupd, 14) {
		t.Fatal("turn_ended missing")
	}
}

func fieldBytesOK(fs []field, num int) ([]byte, bool) {
	b := fieldBytes(fs, num)
	if len(b) == 0 {
		return nil, false
	}
	return b[0], true
}

func connectPayloadsMust(t *testing.T, frame []byte) []byte {
	t.Helper()
	ps, ok := connectPayloads(frame)
	if !ok || len(ps) != 1 {
		t.Fatalf("frame payloads %d ok=%v", len(ps), ok)
	}
	return ps[0]
}

func agentRunBody(model, conv, text string, params []Parameter) []byte {
	return frameOf(agentClient(agentRunMessage(model, conv, text, params)))
}

func agentClient(run []byte) []byte {
	var msg wbuf
	msg.msg(1, run)
	return []byte(msg)
}

func agentRunMessage(model, conv, text string, params []Parameter) []byte {
	var user wbuf
	user.str(1, text)
	user.str(2, "m1")
	var action wbuf
	action.msg(1, []byte(user))
	var uma wbuf
	uma.msg(1, []byte(action))
	var requested wbuf
	requested.str(1, model)
	for _, p := range params {
		var one wbuf
		one.str(1, p.ID)
		one.str(2, p.Value)
		requested.msg(3, []byte(one))
	}
	var run wbuf
	run.msg(2, []byte(uma))
	run.str(5, conv)
	run.msg(9, []byte(requested))
	return []byte(run)
}

func agentRunDetailsOnly(model, conv, text string) []byte {
	var user wbuf
	user.str(1, text)
	var action wbuf
	action.msg(1, []byte(user))
	var uma wbuf
	uma.msg(1, []byte(action))
	var details wbuf
	details.str(1, model)
	var run wbuf
	run.msg(2, []byte(uma))
	run.msg(3, []byte(details))
	run.str(5, conv)
	return []byte(run)
}
