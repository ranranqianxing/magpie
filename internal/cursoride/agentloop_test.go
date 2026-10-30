package cursoride

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestAgentToolResultsFromPayloadFindsCallID(t *testing.T) {
	tool := []byte(`{"role":"tool","content":[{"type":"tool-result","toolCallId":"call_1","toolName":"Shell","result":"ok"}]}`)
	var state wbuf
	state.msg(1, tool)
	var run wbuf
	run.msg(1, []byte(state))
	payload := agentClient([]byte(run))
	want := map[string]bool{"call_1": true}
	got := agentToolResultsFromPayload(payload, want)
	if len(got) != 1 || got[0].ToolCallID != "call_1" || got[0].Content.(string) != "ok" {
		t.Fatalf("got %+v", got)
	}
}

func TestAgentGatewayRoundWaitsWhenStreamEndsWithToolsOnly(t *testing.T) {
	dir := t.TempDir()
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x","function":{"name":"Shell","arguments":"{\"command\":\"pwd\"}"}}]}}]}`+"\n\n")
	}))
	defer gw.Close()
	p := &Proxy{
		CADir:   dir,
		Gateway: func() (string, string) { return gw.URL, "tok" },
		client:  http.DefaultClient,
	}
	var execN uint32
	sealed, done, err := p.agentGatewayRound(context.Background(), Chat{
		Model: "codex/gpt-5.6-luna", Messages: []ChatMessage{{Role: "user", Content: "go"}},
	}, func([]byte) {}, &execN)
	if err != nil || done || len(sealed) != 1 || sealed[0].id != "call_x" {
		t.Fatalf("sealed=%+v done=%v err=%v", sealed, done, err)
	}
}

func TestAgentGatewayRoundFillsEmptyCallFromDSML(t *testing.T) {
	// DeepSeek V4 writes the call as DSML and the relay also emits a
	// native tool_call whose arguments are {}. The path is in the text
	// the UI shows; the Pi exec has to carry it.
	const path = "/tmp/magpie-p0-verify/README.md"
	dsml := "<｜DSML｜tool_calls>\n<｜DSML｜invoke name=\"Read\">\n<｜DSML｜parameter name=\"path\" string=\"true\">" + path + "</｜DSML｜parameter>\n</｜DSML｜invoke>\n</｜DSML｜tool_calls>"
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"delta":{"content":`+jsonQuote(dsml)+`}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_r","function":{"name":"Read","arguments":"{}"}}]}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer gw.Close()
	p := &Proxy{
		CADir:   t.TempDir(),
		Gateway: func() (string, string) { return gw.URL, "tok" },
		client:  http.DefaultClient,
	}
	var frames [][]byte
	sealed, done, err := p.agentGatewayRound(context.Background(), Chat{
		Model: "deepseek/deepseek-v4.1-flash", Messages: []ChatMessage{{Role: "user", Content: "read it"}},
	}, func(b []byte) { frames = append(frames, append([]byte(nil), b...)) }, new(uint32))
	if err != nil || done || len(sealed) != 1 || sealed[0].name != "Read" || sealed[0].args != `{"path":"`+path+`"}` {
		t.Fatalf("sealed=%+v done=%v err=%v", sealed, done, err)
	}
	var saw bool
	for _, frame := range frames {
		msg, ok := readExec(frame)
		if !ok {
			continue
		}
		reads := fieldBytes(msg, 45)
		if len(reads) != 1 {
			continue
		}
		body, err := parseFields(reads[0])
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := fieldString(body, 1); got == path {
			saw = true
		}
	}
	if !saw {
		t.Fatal("Pi read exec had no path")
	}
}

func readExec(frame []byte) ([]field, bool) {
	payloads, framed := connectPayloads(frame)
	if !framed || len(payloads) != 1 {
		return nil, false
	}
	top, err := parseFields(payloads[0])
	if err != nil {
		return nil, false
	}
	execs := fieldBytes(top, 2)
	if len(execs) != 1 {
		return nil, false
	}
	msg, err := parseFields(execs[0])
	if err != nil {
		return nil, false
	}
	return msg, true
}

func TestRunAgentLoopContinuesAfterToolResults(t *testing.T) {
	const conv = "66666666-6666-4666-8666-666666666666"
	var mu sync.Mutex
	var bodies [][]byte
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Shell","arguments":"{\"command\":\"ls\"}"}}]}}]}`+"\n\n")
			io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"done"}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer gw.Close()
	dir := t.TempDir()
	p := &Proxy{
		Addr: "127.0.0.1:0", CADir: dir,
		Gateway: func() (string, string) { return gw.URL, "tok" },
		Models:  func() []Model { return agentCatalog() },
		client:  http.DefaultClient,
	}
	turn, ok, err := readAgentClient(agentClient(agentRunMessage("codex/gpt-5.6-luna", conv, "go", nil)))
	if err != nil || !ok {
		t.Fatalf("turn ok=%v err=%v", ok, err)
	}
	route, ours, err := RouteModel(agentCatalog(), turn.ModelID, turn.Params)
	if err != nil || !ours {
		t.Fatal(route, ours, err)
	}
	execSeen := make(chan struct{})
	var out bytes.Buffer
	write := func(b []byte) {
		out.Write(b)
		if agentExecRan(b, "ls") {
			select {
			case <-execSeen:
			default:
				close(execSeen)
			}
		}
	}
	followups := make(chan []byte, 1)
	go func() {
		<-execSeen
		followups <- mustFirstFrame(t, frameOf(agentToolResultClient("call_1", "listed")))
		close(followups)
	}()
	p.runAgentLoop(context.Background(), followups, turn, route, write)
	if !strings.Contains(out.String(), "done") {
		t.Fatalf("stream %q", out.Bytes())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("gateway rounds %d", len(bodies))
	}
	var second struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			Content    string `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(bodies[1], &second) != nil {
		t.Fatalf("second body %s", bodies[1])
	}
	var sawTool bool
	for _, m := range second.Messages {
		if m.Role == "tool" && m.ToolCallID == "call_1" && m.Content == "listed" {
			sawTool = true
		}
	}
	if !sawTool {
		t.Fatalf("second chat %+v", second.Messages)
	}
}

func agentToolResultClient(callID, result string) []byte {
	tool := []byte(fmt.Sprintf(`{"role":"tool","content":[{"type":"tool-result","toolCallId":%q,"toolName":"Shell","result":%q}]}`, callID, result))
	var state wbuf
	state.msg(1, tool)
	var run wbuf
	run.msg(1, []byte(state))
	return agentClient([]byte(run))
}

func mustFirstFrame(t *testing.T, framed []byte) []byte {
	t.Helper()
	ps, ok := connectPayloads(framed)
	if !ok || len(ps) != 1 {
		t.Fatal("frame")
	}
	return ps[0]
}
