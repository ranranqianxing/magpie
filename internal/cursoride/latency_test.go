package cursoride

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestInvokePrefixIsCompiledOnce is the history-bloat case: the next
// tool round sends the same prefix again. The second compile reuses it.
// Clearing the cached JSON and marshaling again must match, including a
// picture that arrived with line breaks. A message added after compile
// (the skipped-tool error) stays on the body.
func TestInvokePrefixIsCompiledOnce(t *testing.T) {
	resetInvokeCache()
	png := append(tinyPNG(), bytes.Repeat([]byte{0x5a}, 4096)...)
	wrapped := wrapB64(base64.StdEncoding.EncodeToString(png))
	const prefix = "latency-prefix-unique"
	lead := []wbuf{
		userImage(prefix, wrapped, "image/png"),
		textMsg(2, "latency-answer-unique"),
	}
	payload := invoke("inv-cache", lead...)
	first, err := CompileChat(Route{Model: "deepseek/deepseek-v4.1-flash"}, "conv", payload)
	if err != nil {
		t.Fatal(err)
	}
	if first.cacheHit != 0 || first.cacheMiss != 2 {
		t.Fatalf("first compile hit=%d miss=%d", first.cacheHit, first.cacheMiss)
	}
	cached, err := first.JSON()
	if err != nil {
		t.Fatal(err)
	}
	first.wire = nil
	fresh, err := first.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cached, fresh) {
		t.Fatalf("cached json differs\n%s\n%s", cached, fresh)
	}
	if got := dataURLBytes(t, string(cached), "image/png"); !bytes.Equal(got, png) {
		t.Fatalf("png %d want %d", len(got), len(png))
	}
	second, err := CompileChat(Route{Model: "deepseek/deepseek-v4.1-flash"}, "conv", payload)
	if err != nil {
		t.Fatal(err)
	}
	if second.cacheHit != 2 || second.cacheMiss != 0 {
		t.Fatalf("prefix compiled again hit=%d miss=%d", second.cacheHit, second.cacheMiss)
	}
	again, err := second.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cached, again) {
		t.Fatal("reused prefix json changed")
	}
	second.Messages = append(second.Messages, ChatMessage{Role: "tool", ToolCallID: "call-x", Content: "missing path"})
	withTail, err := second.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(withTail, []byte(prefix)) || !bytes.Contains(withTail, []byte("missing path")) {
		t.Fatalf("tail dropped: %s", withTail)
	}
	if strings.Contains(string(withTail), "\\u003c") {
		t.Fatalf("html escaped: %s", withTail)
	}
	var got struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(withTail, &got) != nil || len(got.Messages) != 3 {
		t.Fatalf("messages %s", withTail)
	}
	// The next invoke is the same prefix bytes plus one new tool result,
	// which is what a later BidiAppend round sends.
	withResult := invoke("inv-next", lead[0], lead[1], toolResultValueMsg("call_1", map[string]any{"content": "latency-new-result"}))
	third, err := CompileChat(Route{Model: "deepseek/deepseek-v4.1-flash"}, "conv", withResult)
	if err != nil {
		t.Fatal(err)
	}
	if third.cacheHit != 2 || third.cacheMiss != 1 {
		t.Fatalf("prefix hit / new result miss hit=%d miss=%d", third.cacheHit, third.cacheMiss)
	}
	round, err := third.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(round, []byte(prefix)) || !bytes.Contains(round, []byte("latency-new-result")) {
		t.Fatalf("round dropped prefix or result: %s", round)
	}
}

func TestEmptyInferenceMessageIsNotCached(t *testing.T) {
	resetInvokeCache()
	var empty wbuf
	empty.u32(1, 1)
	const text = "kept-beside-empty-latency"
	payload := invoke("inv-empty", empty, textMsg(1, text))
	first, err := CompileChat(Route{Model: "deepseek/deepseek-v4.1-flash"}, "conv", payload)
	if err != nil {
		t.Fatal(err)
	}
	if first.cacheHit != 0 || first.cacheMiss != 2 {
		t.Fatalf("first hit=%d miss=%d", first.cacheHit, first.cacheMiss)
	}
	second, err := CompileChat(Route{Model: "deepseek/deepseek-v4.1-flash"}, "conv", payload)
	if err != nil {
		t.Fatal(err)
	}
	if second.cacheHit != 1 || second.cacheMiss != 1 {
		t.Fatalf("empty message was cached hit=%d miss=%d", second.cacheHit, second.cacheMiss)
	}
	body, err := second.JSON()
	if err != nil || !bytes.Contains(body, []byte(text)) {
		t.Fatalf("body %s err=%v", body, err)
	}
}

// TestBidiStreamsTextBeforeSealAndLogsTiming holds the gateway after the
// first text delta. The inference stream must already contain that text,
// and not the tool path, before the rest of the body is written. The
// assumption is local: the held write is released only after the SSE
// client has seen the text. If text waited for seal, the gateway stays
// blocked until this test's context ends (8s) and the test fails. The
// timing lines are durations and counts. The typed text is not on them.
func TestBidiStreamsTextBeforeSealAndLogsTiming(t *testing.T) {
	const model = "deepseek/deepseek-v4.1-flash"
	const conv = "11111111-1111-4111-8111-111111111111"
	const text = "latency-hello-token"
	const path = "latency-read-target.go"
	t.Setenv("MAGPIE_DEBUG", "1")
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	release := make(chan struct{})
	var gotEnc string
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		gotEnc = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"`+text+`"}}]}`+"\n\n")
		if fl != nil {
			fl.Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		obj := `{"path":"` + path + `"}`
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_r","type":"function","function":{"name":"Read","arguments":`+obj+`}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
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

	const reqID = "req-latency"
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
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
	postCursor(t, client, "/aiserver.v1.BidiService/BidiAppend", bidiAppend(reqID, invoke("inv-lat", textMsg(1, text))))
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
	var early []byte
	tmp := make([]byte, 1024)
	finished := false
	signaled := false
	for {
		n, rerr := resp.Body.Read(tmp)
		stream.Write(tmp[:n])
		if !signaled && bytes.Contains(stream.Bytes(), []byte(text)) {
			early = append([]byte(nil), stream.Bytes()...)
			signaled = true
			close(release)
		}
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
			t.Fatal("text did not stream before the tool seal, or the stream stalled")
		}
	}
	if !signaled || bytes.Contains(early, []byte(path)) {
		t.Fatalf("text was not ahead of the sealed tool\nearly=%q", early)
	}
	parts := toolPartsFromInference(t, stream.Bytes())
	if len(parts) != 1 || !parts[0].complete || !strings.Contains(parts[0].args, path) {
		t.Fatalf("sealed parts %+v", parts)
	}
	if gotEnc != "identity" {
		t.Fatalf("Accept-Encoding %q", gotEnc)
	}
	out := logged.String()
	for _, key := range []string{"compile-ms=", "gateway-ttfb=", "seal-ms=", "append-ms=", "json-ms=", "first-text-ms="} {
		if !strings.Contains(out, key) {
			t.Fatalf("missing %s\n%s", key, out)
		}
	}
	if strings.Contains(out, text) || strings.Contains(out, path) {
		t.Fatalf("timing line kept text or a path:\n%s", out)
	}
}
