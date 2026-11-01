package cursoride

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Byok's Agent composer is this loop. The invoke inlines the transcript.
// Official InferenceToolCall args are a protobuf Struct, and the tool
// result is a protobuf Value. The gateway body has to carry both, and
// the diagnostic has to say the path was BidiAppend with inline history.
func TestBidiAppendReplaysInlineHistory(t *testing.T) {
	t.Setenv("MAGPIE_DEBUG", "1")
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	const conv = "11111111-1111-4111-8111-111111111111"
	const model = "deepseek/deepseek-v4.1-flash"
	const token = "early-result-token"
	gotChat := make(chan []byte, 1)
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		gotChat <- b
	})
	up := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("forwarded %s", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	})
	dir := t.TempDir()
	p := &Proxy{
		Addr: "127.0.0.1:0", CADir: dir, Upstream: up.URL,
		Gateway: func() (string, string) { return gw.URL, "tok" },
		Models:  func() []Model { return []Model{{ID: model, Name: "Flash"}} },
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	client := cursorClient(t, strings.TrimPrefix(addr, "http://"), dir, "api2.cursor.sh")

	call := withReasoning(assistantStructCall("read it", "call_1", "Read", "", map[string]any{"path": "a.go"}), "kept-the-thought")
	payload := invoke("inv-long",
		call,
		toolResultValueMsg("call_1", map[string]any{"content": token}),
		textMsg(1, "what was in the file"),
	)
	inferenceInvoke(t, client, conv, model, payload)

	var chat []byte
	select {
	case chat = <-gotChat:
	default:
		t.Fatal("gateway was not asked")
	}
	if !bytes.Contains(chat, []byte(`path\":\"a.go`)) && !bytes.Contains(chat, []byte("a.go")) {
		t.Fatalf("struct args were not replayed: %s", chat)
	}
	if !bytes.Contains(chat, []byte(token)) || !bytes.Contains(chat, []byte("kept-the-thought")) {
		t.Fatalf("result or reasoning dropped: %s", chat)
	}
	var got struct {
		Messages []struct {
			Reasoning string `json:"reasoning_content"`
		} `json:"messages"`
	}
	if json.Unmarshal(chat, &got) != nil || len(got.Messages) != 3 || got.Messages[0].Reasoning != "kept-the-thought" {
		t.Fatalf("chat %s", chat)
	}
	logged := buf.String()
	if strings.Contains(logged, token) || strings.Contains(logged, "kept-the-thought") || strings.Contains(logged, "a.go") {
		t.Fatalf("diagnostic kept the transcript:\n%s", logged)
	}
	want := "chat start model=" + model + " path=BidiAppend history=inline messages=3 tools=0 calls=1 results=1 images=0 bytes="
	if !strings.Contains(logged, want) || !strings.Contains(logged, "empty-args=0") || !strings.Contains(logged, "reasoning=1") {
		t.Fatalf("inline history was not logged:\n%s", logged)
	}
	if strings.Contains(logged, "path=AgentService/Run") || strings.Contains(logged, "agent kv hydrate") {
		t.Fatalf("this invoke took the Run path:\n%s", logged)
	}
}

func inferenceInvoke(t *testing.T, client *http.Client, conv, model string, payload []byte) {
	t.Helper()
	const reqID = "req-inline"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api2.cursor.sh/aiserver.v1.InferenceService/RunInferenceSSE", bytes.NewReader(sseRequest(reqID)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/proto")
	sseResp := make(chan *http.Response, 1)
	sseErr := make(chan error, 1)
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			sseErr <- err
			return
		}
		sseResp <- resp
	}()
	postCursor(t, client, "/aiserver.v1.BidiService/BidiAppend", bidiAppend(reqID, runRequest(conv, model, nil)))
	postCursor(t, client, "/aiserver.v1.BidiService/BidiAppend", bidiAppend(reqID, payload))
	var resp *http.Response
	select {
	case resp = <-sseResp:
	case err := <-sseErr:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("stream did not start")
	}
	defer resp.Body.Close()
	var stream bytes.Buffer
	tmp := make([]byte, 1024)
	finished := false
	for {
		n, rerr := resp.Body.Read(tmp)
		stream.Write(tmp[:n])
		if !finished && inferenceEnded(stream.Bytes()) {
			finished = true
			postCursor(t, client, "/aiserver.v1.BidiService/BidiAppend", bidiAppend(reqID, finishMsg()))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			t.Fatal(rerr)
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
	}
	if !finished {
		t.Fatal("invocation did not end")
	}
}
