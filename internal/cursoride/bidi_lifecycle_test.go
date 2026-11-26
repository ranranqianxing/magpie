package cursoride

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

const lifeModel = "deepseek/deepseek-v4.1-flash"
const lifeConv = "11111111-1111-4111-8111-111111111111"

func newLifeProxy(t *testing.T, gwURL string) (*Proxy, *http.Client) {
	t.Helper()
	up := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("forwarded %s", r.URL.Path)
		w.WriteHeader(http.StatusOK)
	})
	dir := t.TempDir()
	p := &Proxy{
		Addr: "127.0.0.1:0", CADir: dir, Upstream: up.URL,
		Gateway: func() (string, string) { return gwURL, "tok" },
		Models:  func() []Model { return []Model{{ID: lifeModel, Name: "Flash"}} },
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, cursorClient(t, strings.TrimPrefix(addr, "http://"), dir, "api2.cursor.sh")
}

func postLife(t *testing.T, client *http.Client, reqID string, payload []byte) {
	t.Helper()
	postCursor(t, client, "/aiserver.v1.BidiService/BidiAppend", bidiAppend(reqID, payload))
}

func openLifeSSE(t *testing.T, client *http.Client, reqID string) (context.CancelFunc, *http.Response) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api2.cursor.sh/aiserver.v1.InferenceService/RunInferenceSSE", bytes.NewReader(sseRequest(reqID)))
	if err != nil {
		cancel()
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
	postLife(t, client, reqID, runRequest(lifeConv, lifeModel, nil))
	select {
	case resp := <-sseResp:
		return cancel, resp
	case err := <-sseErr:
		cancel()
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("stream did not start")
	}
	return cancel, nil
}

func readUntil(t *testing.T, body io.Reader, hit func([]byte) bool) []byte {
	t.Helper()
	var stream bytes.Buffer
	tmp := make([]byte, 2048)
	deadline := time.Now().Add(5 * time.Second)
	for {
		type one struct {
			n   int
			err error
		}
		ch := make(chan one, 1)
		go func() {
			n, err := body.Read(tmp)
			ch <- one{n, err}
		}()
		select {
		case r := <-ch:
			stream.Write(tmp[:r.n])
			if hit(stream.Bytes()) {
				return stream.Bytes()
			}
			if r.err != nil {
				t.Fatalf("sse ended before the turn: %v\n%s", r.err, stream.Bytes())
			}
		case <-time.After(time.Until(deadline)):
			t.Fatalf("sse timed out\n%s", stream.Bytes())
		}
		if time.Now().After(deadline) {
			t.Fatalf("sse timed out\n%s", stream.Bytes())
		}
	}
}

func invocationEnds(body []byte) int {
	b := body
	n := 0
	for len(b) >= 5 {
		flags := b[0]
		if flags > 3 {
			return n
		}
		ln := binary.BigEndian.Uint32(b[1:5])
		if uint64(ln) > uint64(len(b)-5) {
			return n
		}
		payload := b[5 : 5+int(ln)]
		b = b[5+int(ln):]
		if flags&0x02 != 0 {
			continue
		}
		fs, err := parseFields(payload)
		if err != nil {
			continue
		}
		if len(fieldBytes(fs, 4)) > 0 {
			n++
		}
	}
	return n
}

func inferenceErrorText(body []byte) string {
	payloads, ok := connectPayloads(body)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, p := range payloads {
		fs, err := parseFields(p)
		if err != nil {
			continue
		}
		for _, raw := range fieldBytes(fs, 4) {
			ef, err := parseFields(raw)
			if err != nil {
				continue
			}
			for _, er := range fieldBytes(ef, 2) {
				erf, err := parseFields(er)
				if err != nil {
					continue
				}
				msg, _ := fieldString(erf, 2)
				if msg != "" {
					b.WriteString(msg)
					b.WriteByte('\n')
				}
			}
		}
	}
	return b.String()
}

func waitHubEmpty(t *testing.T, p *Proxy) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		p.hub.mu.Lock()
		n := len(p.hub.m)
		p.hub.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session still held: %d", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBidiStreamErrorDoesNotRunTheTool(t *testing.T) {
	const path = "/tmp/magpie-stream-error/README.md"
	obj := `{"path":"` + path + `"}`
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_r","function":{"name":"Read","arguments":`+obj+`}}]}}]}`+"\n\n")
		io.WriteString(w, `data: {"error":{"message":"rate limited"}}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	_, client := newLifeProxy(t, gw.URL)
	body := inferenceRound(t, client, lifeConv, lifeModel, "read it")
	if parts := toolPartsFromInference(t, body); len(parts) != 0 {
		t.Fatalf("tool ran after the stream error: %+v", parts)
	}
	if !strings.Contains(inferenceErrorText(body), "rate limited") {
		t.Fatalf("error text missing: %q", inferenceErrorText(body))
	}
}

func TestBidiStreamLineTooLargeDoesNotRunTheTool(t *testing.T) {
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"x\":\""+strings.Repeat("a", (4<<20)+8)+"\"}\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_r\",\"function\":{\"name\":\"Read\",\"arguments\":\"{\\\"path\\\":\\\"/tmp/too-big\\\"}\"}}]}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	_, client := newLifeProxy(t, gw.URL)
	body := inferenceRound(t, client, lifeConv, lifeModel, "read it")
	if parts := toolPartsFromInference(t, body); len(parts) != 0 {
		t.Fatalf("tool ran after the scanner stopped: %+v", parts)
	}
	if !strings.Contains(inferenceErrorText(body), "too large") {
		t.Fatalf("scanner error missing: %q", inferenceErrorText(body))
	}
}

func TestBidiCancelStopsTheRunAndALaterInvokeStillWorks(t *testing.T) {
	const path = "/tmp/magpie-cancel/README.md"
	obj := `{"path":"` + path + `"}`
	entered := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var firstCanceled bool
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		if strings.Contains(string(body), "after-cancel") {
			io.WriteString(w, `data: {"choices":[{"delta":{"content":"after-cancel-ok"}}]}`+"\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
			if fl != nil {
				fl.Flush()
			}
			return
		}
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_r","function":{"name":"Read","arguments":`+obj+`}}]}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"seen-before-cancel"}}]}`+"\n\n")
		if fl != nil {
			fl.Flush()
		}
		once.Do(func() { close(entered) })
		select {
		case <-r.Context().Done():
			mu.Lock()
			firstCanceled = true
			mu.Unlock()
		case <-time.After(3 * time.Second):
			t.Errorf("cancel did not reach the gateway")
		}
	})
	_, client := newLifeProxy(t, gw.URL)
	const reqID = "req-cancel"
	cancelSSE, resp := openLifeSSE(t, client, reqID)
	defer cancelSSE()
	defer resp.Body.Close()
	postLife(t, client, reqID, invoke("inv-1", textMsg(1, "read it")))
	_ = readUntil(t, resp.Body, func(b []byte) bool {
		return bytes.Contains(b, []byte("seen-before-cancel"))
	})
	<-entered
	postLife(t, client, reqID, cancelMsg("inv-1"))
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		ok := firstCanceled
		mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gateway context was not canceled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	first := readUntil(t, resp.Body, func(b []byte) bool { return invocationEnds(b) >= 1 })
	if parts := toolPartsFromInference(t, first); len(parts) != 0 {
		t.Fatalf("tool ran after cancel: %+v", parts)
	}
	postLife(t, client, reqID, invoke("inv-2", textMsg(1, "after-cancel")))
	second := readUntil(t, resp.Body, func(b []byte) bool {
		return bytes.Contains(b, []byte("after-cancel-ok")) && invocationEnds(b) >= 1
	})
	if parts := toolPartsFromInference(t, second); len(parts) != 0 {
		t.Fatalf("later turn ran the canceled tool: %+v", parts)
	}
}

func TestRunInferenceSSEReconnectDoesNotCancelTheGateway(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	canceled := false
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"hello-1"}}]}`+"\n\n")
		if fl != nil {
			fl.Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
			mu.Lock()
			canceled = true
			mu.Unlock()
			return
		}
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"hello-2"}}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	})
	_, client := newLifeProxy(t, gw.URL)
	const reqID = "req-reconnect"
	cancel1, resp1 := openLifeSSE(t, client, reqID)
	postLife(t, client, reqID, invoke("inv-1", textMsg(1, "hi")))
	_ = readUntil(t, resp1.Body, func(b []byte) bool { return bytes.Contains(b, []byte("hello-1")) })
	cancel1()
	_ = resp1.Body.Close()
	cancel2, resp2 := openLifeSSE(t, client, reqID)
	defer cancel2()
	defer resp2.Body.Close()
	mu.Lock()
	stopped := canceled
	mu.Unlock()
	if stopped {
		t.Fatal("reconnecting RunInferenceSSE canceled the gateway call")
	}
	close(release)
	body := readUntil(t, resp2.Body, func(b []byte) bool {
		return bytes.Contains(b, []byte("hello-2")) && invocationEnds(b) >= 1
	})
	mu.Lock()
	stopped = canceled
	mu.Unlock()
	if stopped {
		t.Fatal("gateway was canceled while the second stream was reading")
	}
	if !bytes.Contains(body, []byte("hello-2")) {
		t.Fatalf("reconnect missed the rest: %s", body)
	}
	postLife(t, client, reqID, finishMsg())
}

func TestBidiFollowUpInvokeKeepsTheSameStream(t *testing.T) {
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Context().Err() != nil {
			t.Errorf("turn started canceled")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		msg := "turn-one-ok"
		if strings.Contains(string(body), "turn-two") {
			msg = "turn-two-ok"
		}
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"`+msg+`"}}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	})
	_, client := newLifeProxy(t, gw.URL)
	const reqID = "req-turns"
	cancel, resp := openLifeSSE(t, client, reqID)
	defer cancel()
	defer resp.Body.Close()
	postLife(t, client, reqID, invoke("inv-1", textMsg(1, "turn-one")))
	_ = readUntil(t, resp.Body, func(b []byte) bool {
		return bytes.Contains(b, []byte("turn-one-ok")) && invocationEnds(b) >= 1
	})
	postLife(t, client, reqID, invoke("inv-2", textMsg(1, "turn-two")))
	body := readUntil(t, resp.Body, func(b []byte) bool {
		return bytes.Contains(b, []byte("turn-two-ok")) && invocationEnds(b) >= 1
	})
	if !bytes.Contains(body, []byte("turn-two-ok")) {
		t.Fatalf("second turn: %s", body)
	}
	postLife(t, client, reqID, finishMsg())
}

func TestFinishedBidiRequestCanStartAgain(t *testing.T) {
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"again-ok"}}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	p, client := newLifeProxy(t, gw.URL)
	body := inferenceRound(t, client, lifeConv, lifeModel, "first")
	if !bytes.Contains(body, []byte("again-ok")) {
		t.Fatalf("first round: %s", body)
	}
	waitHubEmpty(t, p)
	body = inferenceRound(t, client, lifeConv, lifeModel, "second")
	if !bytes.Contains(body, []byte("again-ok")) {
		t.Fatalf("reused request id dropped the second round: %s", body)
	}
}

func TestProxyCloseCancelsTheGatewayRun(t *testing.T) {
	entered := make(chan struct{})
	canceled := make(chan struct{})
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(entered)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-time.After(2 * time.Second):
		}
	})
	p, client := newLifeProxy(t, gw.URL)
	const reqID = "req-close"
	cancel, resp := openLifeSSE(t, client, reqID)
	defer cancel()
	defer resp.Body.Close()
	postLife(t, client, reqID, invoke("inv-1", textMsg(1, "hi")))
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("gateway was not asked")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("closing the proxy left the gateway call running")
	}
}

func cancelMsg(inv string) []byte {
	var inner wbuf
	inner.str(1, inv)
	var client wbuf
	client.msg(3, inner)
	return []byte(client)
}

func TestChatPiecesReportsStreamError(t *testing.T) {
	p, ok := chatPieces(`data: {"error":{"message":"rate limited"}}`)
	if !ok || p.fail != "rate limited" || p.text != "" || len(p.tools) != 0 {
		t.Fatalf("ok=%v piece=%+v", ok, p)
	}
	p, ok = chatPieces(`data: {"choices":[{"delta":{"content":"hi"}}],"error":{"message":"nope"}}`)
	if !ok || p.fail != "nope" || p.text != "" {
		t.Fatalf("error did not win the line: %+v", p)
	}
	if _, ok = chatPieces(`data: {"error":null}`); ok {
		t.Fatal("null error became a piece")
	}
}

func TestPushRunStopsAtTheFrameCapUntilRead(t *testing.T) {
	h := newHub()
	s := h.sess("cap")
	_, gen := s.beginRun("inv")
	errCh := make(chan error, 1)
	go func() {
		for i := 0; i < frameQueueMax+1; i++ {
			if !s.pushRun(gen, textFrame("inv", "x", false)) {
				errCh <- errf("push %d", i)
				return
			}
		}
		errCh <- nil
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.frames)
		s.mu.Unlock()
		if n > frameQueueMax {
			t.Fatalf("queue grew to %d", n)
		}
		if n == frameQueueMax {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue stopped at %d", n)
		}
		time.Sleep(time.Millisecond)
	}
	watch, detach := s.attach()
	defer detach()
	if _, ok := s.next(context.Background(), watch); !ok {
		t.Fatal("reader got nothing")
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pusher stayed blocked after a frame was read")
	}
	s.mu.Lock()
	n := len(s.frames)
	s.mu.Unlock()
	if n > frameQueueMax {
		t.Fatalf("queue %d", n)
	}
}

func TestAgentGatewayRoundDoesNotExecuteOnStreamError(t *testing.T) {
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_r","function":{"name":"Read","arguments":"{\"path\":\"/tmp/x\"}"}}]}}]}`+"\n\n")
		io.WriteString(w, `data: {"error":{"message":"rate limited"}}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	p := &Proxy{Gateway: func() (string, string) { return gw.URL, "tok" }, client: http.DefaultClient}
	var execN uint32
	var execs int
	sealed, _, _, err := p.agentGatewayRound(context.Background(), Chat{
		Model: "deepseek/deepseek-v4.1-flash", Messages: []ChatMessage{{Role: "user", Content: "read"}},
	}, func(b []byte) {
		if _, ok := readExec(b); ok {
			execs++
		}
	}, &execN)
	if err == nil || !strings.Contains(err.Error(), "rate limited") || len(sealed) != 0 || execs != 0 {
		t.Fatalf("sealed=%+v execs=%d err=%v", sealed, execs, err)
	}
}

func TestAgentGatewayRoundStopsWhenTheContextIsCanceled(t *testing.T) {
	entered := make(chan struct{})
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_r","function":{"name":"Read","arguments":"{\"path\":\"/tmp/x\"}"}}]}}]}`+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(entered)
		<-r.Context().Done()
	})
	p := &Proxy{Gateway: func() (string, string) { return gw.URL, "tok" }, client: http.DefaultClient}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-entered
		cancel()
	}()
	sealed, _, _, err := p.agentGatewayRound(ctx, Chat{
		Model: "deepseek/deepseek-v4.1-flash", Messages: []ChatMessage{{Role: "user", Content: "read"}},
	}, func([]byte) {}, new(uint32))
	if !errors.Is(err, errStopped) || len(sealed) != 0 {
		t.Fatalf("sealed=%+v err=%v", sealed, err)
	}
}
