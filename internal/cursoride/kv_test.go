package cursoride

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHydrateRestoresTheRootPromptAndKeepsAStrayFrame(t *testing.T) {
	turn, store := longSessionTurn(t)
	if !strings.Contains(messageString(turn.Messages), "from-turn") || strings.Contains(messageString(turn.Messages), "workspace-alpha") {
		t.Fatalf("before hydrate %+v", turn.Messages)
	}
	if turn.MissingBlobs < 4 {
		t.Fatalf("missing %d, root holes were hidden behind turns", turn.MissingBlobs)
	}
	follow := make(chan []byte, 8)
	follow <- heartbeatPayload()
	follow <- strayExecPayload()
	src := &followSource{ch: follow}
	turn.liveKV = true
	got := hydrateAgentTurn(context.Background(), turn, func(frame []byte) {
		id, blob, ok := parseKVGet(frame)
		if !ok {
			t.Errorf("not a get")
			return
		}
		follow <- kvResultPayload(id, store[hex.EncodeToString(blob)])
	}, src)
	text := messageString(got.Messages)
	if !strings.Contains(text, "workspace-alpha") || !strings.Contains(text, "package handler") || !strings.Contains(text, "Earlier the handler lived in a.go.") {
		t.Fatalf("prompt %s", text)
	}
	if strings.Contains(text, "from-turn") || strings.Contains(text, "thin-output") {
		t.Fatalf("turns stand-in stayed in the prompt: %s", text)
	}
	if got.Messages[0].Role != "system" || got.Messages[len(got.Messages)-1].Content.(string) != "continue" {
		t.Fatalf("ends %+v ... %+v", got.Messages[0], got.Messages[len(got.Messages)-1])
	}
	var sawCall bool
	for _, m := range got.Messages {
		if len(m.ToolCalls) == 1 && m.ToolCalls[0].ID == "c1" && strings.Contains(m.ToolCalls[0].Arguments, "a.go") {
			sawCall = true
		}
	}
	if !sawCall {
		t.Fatalf("tool call missing %+v", got.Messages)
	}
	if !strings.Contains(Recent(), "agent kv hydrate requested=4 filled=4") {
		t.Fatalf("trace:\n%s", Recent())
	}
	stray, ok := src.take()
	if !ok || !bytes.Equal(stray, strayExecPayload()) {
		t.Fatalf("stray frame was consumed (%v)", ok)
	}
}

func TestHydrateDoesNotAskWhenTheContextIsCancelled(t *testing.T) {
	turn, _ := longSessionTurn(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	turn.liveKV = true
	got := hydrateAgentTurn(ctx, turn, func([]byte) {
		t.Fatal("cancelled ask wrote a get")
	}, &followSource{ch: make(chan []byte)})
	if !strings.Contains(messageString(got.Messages), "from-turn") {
		t.Fatalf("prompt changed %+v", got.Messages)
	}
}

func TestHydrateKeepsTurnsWhenTheClientHasNoBlob(t *testing.T) {
	turn, _ := longSessionTurn(t)
	follow := make(chan []byte, 8)
	turn.liveKV = true
	got := hydrateAgentTurn(context.Background(), turn, func(frame []byte) {
		id, _, ok := parseKVGet(frame)
		if !ok {
			t.Fatal("not a get")
		}
		follow <- kvResultPayload(id, nil)
	}, &followSource{ch: follow})
	text := messageString(got.Messages)
	if !strings.Contains(text, "from-turn") || strings.Contains(text, "workspace-alpha") {
		t.Fatalf("prompt %s", text)
	}
	if !strings.Contains(Recent(), "agent kv hydrate requested=4 filled=0") {
		t.Fatalf("trace:\n%s", Recent())
	}
}

func TestHydrateSkipsABrokenJSONBlobAndKeepsItsSibling(t *testing.T) {
	idBad := bytes.Repeat([]byte{8}, 32)
	idGood := bytes.Repeat([]byte{9}, 32)
	var state wbuf
	state.msg(1, idBad)
	state.msg(1, idGood)
	turn := turnFromState(t, []byte(state), "continue")
	store := map[string][]byte{
		hex.EncodeToString(idBad):  []byte(`{"role"`),
		hex.EncodeToString(idGood): []byte(`{"role":"user","content":"kept-turn"}`),
	}
	follow := make(chan []byte, 4)
	turn.liveKV = true
	got := hydrateAgentTurn(context.Background(), turn, func(frame []byte) {
		id, blob, ok := parseKVGet(frame)
		if !ok {
			t.Fatal("not a get")
		}
		follow <- kvResultPayload(id, store[hex.EncodeToString(blob)])
	}, &followSource{ch: follow})
	text := messageString(got.Messages)
	if !strings.Contains(text, "kept-turn") || strings.Contains(text, `{"role"`) {
		t.Fatalf("prompt %s", text)
	}
	if !strings.Contains(Recent(), "filled=1 skipped=1") {
		t.Fatalf("trace:\n%s", Recent())
	}
}

func TestHydrateReadsATurnWhoseStepIsAlsoABlob(t *testing.T) {
	idTurn := bytes.Repeat([]byte{5}, 32)
	idStep := bytes.Repeat([]byte{6}, 32)
	var um wbuf
	um.str(1, "fix nested")
	var agent wbuf
	agent.msg(1, []byte(um))
	agent.msg(2, idStep)
	var turnMsg wbuf
	turnMsg.msg(1, []byte(agent))
	var state wbuf
	state.msg(8, idTurn)
	store := map[string][]byte{
		hex.EncodeToString(idTurn): []byte(turnMsg),
		hex.EncodeToString(idStep): agentReadStep("package nested", "c-nest"),
	}
	turn := turnFromState(t, []byte(state), "continue")
	follow := make(chan []byte, 4)
	turn.liveKV = true
	got := hydrateAgentTurn(context.Background(), turn, func(frame []byte) {
		id, blob, ok := parseKVGet(frame)
		if !ok {
			t.Fatal("not a get")
		}
		follow <- kvResultPayload(id, store[hex.EncodeToString(blob)])
	}, &followSource{ch: follow})
	text := messageString(got.Messages)
	if !strings.Contains(text, "fix nested") || !strings.Contains(text, "package nested") {
		t.Fatalf("prompt %s", text)
	}
	var saw bool
	for _, m := range got.Messages {
		if len(m.ToolCalls) == 1 && m.ToolCalls[0].ID == "c-nest" && strings.Contains(m.ToolCalls[0].Arguments, "a.go") {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("nested call missing %+v", got.Messages)
	}
	if !strings.Contains(Recent(), "agent kv hydrate requested=2 filled=2") {
		t.Fatalf("trace:\n%s", Recent())
	}
}

func TestLiveRunAsksForBlobsBeforeTheGateway(t *testing.T) {
	turn, store := longSessionTurn(t)
	var mu sync.Mutex
	var body []byte
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = append([]byte(nil), b...)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"answered"}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	p := &Proxy{
		Gateway: func() (string, string) { return gw.URL, "tok" },
		Models:  func() []Model { return agentCatalog() },
		client:  http.DefaultClient,
	}
	bodyR, bodyW := io.Pipe()
	defer bodyW.Close()
	req := httptest.NewRequest(http.MethodPost, "https://api2.cursor.sh"+PathAgentRun, bodyR)
	req.ContentLength = -1
	req.Header.Set("Transfer-Encoding", "chunked")
	sw := &streamResponse{ch: make(chan []byte, 32)}
	done := make(chan struct{})
	go func() {
		p.agentRun(sw, req)
		close(done)
	}()
	if _, err := bodyW.Write(frameOf(agentClient(turn.rawRun))); err != nil {
		t.Fatal(err)
	}
	asked := 0
	var stream bytes.Buffer
	timeout := time.After(5 * time.Second)
	finished := false
	for !finished {
		select {
		case frame := <-sw.ch:
			stream.Write(frame)
			if id, blob, ok := parseKVGet(frame); ok {
				asked++
				if _, err := bodyW.Write(frameOf(kvResultPayload(id, store[hex.EncodeToString(blob)]))); err != nil {
					t.Fatal(err)
				}
			}
			if bytes.Contains(stream.Bytes(), []byte("answered")) {
				// The handler drains the open body after the turn.
				// Closing it lets that drain finish.
				_ = bodyW.Close()
			}
		case <-done:
			finished = true
		case <-timeout:
			t.Fatalf("stream stalled after %d gets %q\n%s", asked, stream.Bytes(), Recent())
		}
	}
	for {
		select {
		case frame := <-sw.ch:
			stream.Write(frame)
		default:
			goto drained
		}
	}
drained:
	if asked != 4 {
		t.Fatalf("gets %d stream %q", asked, stream.Bytes())
	}
	if !bytes.Contains(stream.Bytes(), []byte("answered")) {
		t.Fatalf("stream %q", stream.Bytes())
	}
	mu.Lock()
	defer mu.Unlock()
	chat := string(body)
	if !strings.Contains(chat, "workspace-alpha") || !strings.Contains(chat, "package handler") || !strings.Contains(chat, "Earlier the handler lived in a.go.") {
		t.Fatalf("gateway %s", chat)
	}
	if strings.Contains(chat, "from-turn") || strings.Contains(chat, "thin-output") {
		t.Fatalf("gateway kept the turns stand-in: %s", chat)
	}
}

type streamResponse struct {
	h  http.Header
	ch chan []byte
}

func (s *streamResponse) Header() http.Header {
	if s.h == nil {
		s.h = make(http.Header)
	}
	return s.h
}

func (s *streamResponse) WriteHeader(int) {}

func (s *streamResponse) Write(p []byte) (int, error) {
	s.ch <- append([]byte(nil), p...)
	return len(p), nil
}

func (s *streamResponse) Flush() {}

func longSessionTurn(t *testing.T) (AgentTurn, map[string][]byte) {
	t.Helper()
	idUser := bytes.Repeat([]byte{1}, 32)
	idAsst := bytes.Repeat([]byte{2}, 32)
	idTool := bytes.Repeat([]byte{3}, 32)
	idSum := bytes.Repeat([]byte{4}, 32)
	user := []byte(`{"role":"user","content":[{"type":"text","text":"<user_info>\nworkspace-alpha\n</user_info>\n<user_query>\nfix the handler\n</user_query>"}]}`)
	asst := []byte(`{"role":"assistant","content":[{"type":"text","text":"reading"},{"type":"tool-call","toolCallId":"c1","toolName":"Read","args":{"path":"a.go"}}]}`)
	tool := []byte(`{"role":"tool","content":[{"type":"tool-result","toolCallId":"c1","toolName":"Read","result":"package handler"}]}`)
	summary := []byte("Earlier the handler lived in a.go.")
	var state wbuf
	state.msg(1, idUser)
	state.msg(1, idAsst)
	state.msg(1, idTool)
	state.msg(6, idSum)
	// The stand-in result is long enough that the step protobuf is not
	// itself 32 bytes. A 32-byte step is a blob id, and this one is not.
	state.msg(8, agentTurnWithRead("from-turn", "a.go", "c-turn", "thin-output"))
	store := map[string][]byte{
		hex.EncodeToString(idUser): user,
		hex.EncodeToString(idAsst): asst,
		hex.EncodeToString(idTool): tool,
		hex.EncodeToString(idSum):  summary,
	}
	return turnFromState(t, []byte(state), "continue"), store
}

func turnFromState(t *testing.T, state []byte, text string) AgentTurn {
	t.Helper()
	var user wbuf
	user.str(1, text)
	var action wbuf
	action.msg(1, []byte(user))
	var uma wbuf
	uma.msg(1, []byte(action))
	var requested wbuf
	requested.str(1, "codex/gpt-5.6-luna")
	var run wbuf
	run.msg(1, state)
	run.msg(2, []byte(uma))
	run.str(5, "conv-long")
	run.msg(9, []byte(requested))
	got, ok, err := readAgentClient(agentClient([]byte(run)))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	return got
}

func messageString(msgs []ChatMessage) string {
	var b strings.Builder
	for _, m := range msgs {
		if s, ok := messageText(m); ok {
			b.WriteString(s)
			b.WriteByte('\n')
		}
		for _, c := range m.ToolCalls {
			b.WriteString(c.Arguments)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func parseKVGet(frame []byte) (uint32, []byte, bool) {
	payload := frame
	if ps, ok := connectPayloads(frame); ok && len(ps) == 1 {
		payload = ps[0]
	}
	fs, err := parseFields(payload)
	if err != nil {
		return 0, nil, false
	}
	raw := fieldBytes(fs, 4)
	if len(raw) == 0 {
		return 0, nil, false
	}
	kf, err := parseFields(raw[0])
	if err != nil {
		return 0, nil, false
	}
	n, ok := fieldUint(kf, 1)
	args := fieldBytes(kf, 2)
	if !ok || len(args) == 0 {
		return 0, nil, false
	}
	af, err := parseFields(args[0])
	if err != nil {
		return 0, nil, false
	}
	ids := fieldBytes(af, 1)
	if len(ids) == 0 {
		return 0, nil, false
	}
	return uint32(n), ids[0], true
}

func kvResultPayload(id uint32, data []byte) []byte {
	var result wbuf
	if len(data) > 0 {
		result.msg(1, data)
	}
	var kv wbuf
	kv.u32(1, id)
	kv.msg(2, []byte(result))
	var client wbuf
	client.msg(3, []byte(kv))
	return []byte(client)
}

func heartbeatPayload() []byte {
	var b wbuf
	b.msg(7, nil)
	return []byte(b)
}

func strayExecPayload() []byte {
	var inner wbuf
	inner.u32(1, 9)
	var b wbuf
	b.msg(2, []byte(inner))
	return []byte(b)
}
