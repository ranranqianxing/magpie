package cursoride

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Official Cursor models stay on Cursor's own backend while magpie is
// connected. These tests are the coexistence contract: a catalog id is
// compiled locally, anything else is forwarded as a stream, and a
// successful AvailableModels reply keeps official rows (Grok included).

func TestOfficialInferenceStreamFlushesBeforeUpstreamFinishes(t *testing.T) {
	const conv = "11111111-1111-4111-8111-111111111111"
	release := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	t.Cleanup(open)

	var gwN int
	var gwMu sync.Mutex
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		gwMu.Lock()
		gwN++
		gwMu.Unlock()
		http.Error(w, "gateway", http.StatusBadGateway)
	})
	up := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "RunInferenceSSE") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("G"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
		_, _ = io.WriteString(w, "rok")
	})
	dir := t.TempDir()
	p := &Proxy{
		Addr: "127.0.0.1:0", CADir: dir, Upstream: up.URL,
		Gateway: func() (string, string) { return gw.URL, "tok" },
		Models:  func() []Model { return []Model{{ID: "deepseek/pro", Name: "DeepSeek Pro"}} },
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	client := cursorClient(t, strings.TrimPrefix(addr, "http://"), dir, "api2.cursor.sh")

	reqID := "grok-sse"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api2.cursor.sh/aiserver.v1.InferenceService/RunInferenceSSE", bytes.NewReader(sseRequest(reqID)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/connect+proto")
	type result struct {
		b   []byte
		err error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			got <- result{err: err}
			return
		}
		defer resp.Body.Close()
		buf := make([]byte, 8)
		n, err := resp.Body.Read(buf)
		got <- result{b: append([]byte(nil), buf[:n]...), err: err}
	}()
	postCursor(t, client, "/aiserver.v1.BidiService/BidiAppend", bidiAppend(reqID, runRequest(conv, "grok-4.7", nil)))
	select {
	case res := <-got:
		if res.err != nil && len(res.b) == 0 {
			t.Fatal(res.err)
		}
		if !bytes.Contains(res.b, []byte("G")) {
			t.Fatalf("first bytes %q, want the official stream before it finishes", res.b)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("official RunInferenceSSE was buffered until the upstream finished")
	}
	open()
	gwMu.Lock()
	n := gwN
	gwMu.Unlock()
	if n != 0 {
		t.Fatalf("gateway was asked %d times for official grok", n)
	}
}

func TestOfficialRunSSEFlushesBeforeUpstreamFinishes(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(release) }) }
	t.Cleanup(open)
	var gwN int
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		gwN++
		http.Error(w, "gateway", http.StatusBadGateway)
	})
	up := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent.v1.AgentService/RunSSE" {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer cursor-user" {
			t.Errorf("authorization %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("G"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-release
		_, _ = io.WriteString(w, "rok")
	})
	dir := t.TempDir()
	p := &Proxy{
		Addr: "127.0.0.1:0", CADir: dir, Upstream: up.URL,
		Gateway: func() (string, string) { return gw.URL, "tok" },
		Models:  func() []Model { return []Model{{ID: "deepseek/pro", Name: "DeepSeek Pro"}} },
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	client := cursorClient(t, strings.TrimPrefix(addr, "http://"), dir, "api2.cursor.sh")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api2.cursor.sh/agent.v1.AgentService/RunSSE", bytes.NewReader(sseRequest("grok-run")))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/connect+proto")
	req.Header.Set("Authorization", "Bearer cursor-user")
	got := make(chan []byte, 1)
	errc := make(chan error, 1)
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			errc <- err
			return
		}
		defer resp.Body.Close()
		buf := make([]byte, 8)
		n, _ := resp.Body.Read(buf)
		got <- append([]byte(nil), buf[:n]...)
	}()
	select {
	case b := <-got:
		if !bytes.Contains(b, []byte("G")) {
			t.Fatalf("first bytes %q", b)
		}
	case err := <-errc:
		t.Fatal(err)
	case <-time.After(8 * time.Second):
		t.Fatal("official RunSSE was buffered until the upstream finished")
	}
	open()
	if gwN != 0 {
		t.Fatalf("gateway was asked %d times", gwN)
	}
}

func TestOfficialAgentRunForwardsWhileBodyOpen(t *testing.T) {
	const conv = "44444444-4444-4444-8444-444444444444"
	var gwN int
	var gwMu sync.Mutex
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		gwMu.Lock()
		gwN++
		gwMu.Unlock()
		http.Error(w, "gateway", http.StatusBadGateway)
	})
	saw := make(chan struct{})
	up := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != PathAgentRun {
			w.WriteHeader(http.StatusOK)
			return
		}
		// Cursor's backend writes the run while the request body is
		// still open. A stock net/http server does not, unless asked.
		_ = http.NewResponseController(w).EnableFullDuplex()
		buf := make([]byte, 64<<10)
		n, _ := r.Body.Read(buf)
		if !bytes.Contains(buf[:n], []byte("grok-4.7")) {
			t.Errorf("upstream body %q", buf[:n])
		}
		w.Header().Set("Content-Type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("LIVE"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-saw
		_, _ = io.Copy(io.Discard, r.Body)
	})
	dir := t.TempDir()
	p := &Proxy{
		Addr: "127.0.0.1:0", CADir: dir, Upstream: up.URL,
		Gateway: func() (string, string) { return gw.URL, "tok" },
		Models:  func() []Model { return agentCatalog() },
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	client := cursorClient(t, strings.TrimPrefix(addr, "http://"), dir, "api2.cursor.sh")

	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api2.cursor.sh"+PathAgentRun, pr)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/connect+proto")
	body := agentRunBody("grok-4.7", conv, "hi", nil)
	go func() {
		_, _ = pw.Write(body)
		<-saw
		_ = pw.Close()
	}()
	type result struct {
		b   []byte
		err error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			got <- result{err: err}
			return
		}
		defer resp.Body.Close()
		buf := make([]byte, 8)
		n, err := resp.Body.Read(buf)
		got <- result{b: append([]byte(nil), buf[:n]...), err: err}
	}()
	select {
	case res := <-got:
		if res.err != nil && len(res.b) == 0 {
			t.Fatal(res.err)
		}
		if !bytes.Contains(res.b, []byte("LIVE")) {
			t.Fatalf("first bytes %q, want the official run while the body is open", res.b)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("official AgentService/Run waited for the client to close the body")
	}
	close(saw)
	gwMu.Lock()
	n := gwN
	gwMu.Unlock()
	if n != 0 {
		t.Fatalf("gateway was asked %d times for official grok", n)
	}
}

func TestMagpieCatalogRunStaysLocal(t *testing.T) {
	const conv = "55555555-5555-4555-8555-555555555555"
	var gotModel string
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var msg struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(b, &msg); err != nil {
			t.Errorf("chat %s", b)
		}
		gotModel = msg.Model
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"local\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	up := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PathAgentRun {
			t.Errorf("catalog run was forwarded")
		}
		w.WriteHeader(http.StatusOK)
	})
	dir := t.TempDir()
	p := &Proxy{
		Addr: "127.0.0.1:0", CADir: dir, Upstream: up.URL,
		Gateway: func() (string, string) { return gw.URL, "tok" },
		Models:  func() []Model { return agentCatalog() },
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	client := cursorClient(t, strings.TrimPrefix(addr, "http://"), dir, "api2.cursor.sh")
	body := postCursor(t, client, PathAgentRun, agentRunBody("codex/gpt-5.6-luna", conv, "hi", nil))
	if !bytes.Contains(body, []byte("local")) {
		t.Fatalf("stream %q", body)
	}
	if gotModel != "codex/gpt-5.6-luna" {
		t.Fatalf("gateway model %q", gotModel)
	}
}

func TestAvailableModelsAppendsWithoutDroppingGrok(t *testing.T) {
	official := frameOf(AvailableModels([]Model{{ID: "grok-4.7", Name: "Grok 4.7"}}))
	var enc string
	var mu sync.Mutex
	up := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		enc = r.Header.Get("Accept-Encoding")
		asked := enc
		mu.Unlock()
		if asked != "identity" {
			w.Header().Set("Content-Encoding", "br")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("not-the-official-list"))
			return
		}
		w.Header().Set("Content-Type", "application/proto")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(official)
	})
	dir := t.TempDir()
	p := &Proxy{
		Addr: "127.0.0.1:0", CADir: dir, Upstream: up.URL,
		Gateway: func() (string, string) { return "", "" },
		Models:  func() []Model { return []Model{{ID: "deepseek/pro", Name: "DeepSeek Pro"}} },
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	client := cursorClient(t, strings.TrimPrefix(addr, "http://"), dir, "api2.cursor.sh")
	out := postCursorHeaders(t, client, PathAvailableModels, frameOf(nil), map[string]string{"Accept-Encoding": "br"})
	if !bytes.Contains(out, []byte("grok-4.7")) || !bytes.Contains(out, []byte("Grok 4.7")) {
		t.Fatalf("official Grok dropped from catalog: %q", out)
	}
	if !bytes.Contains(out, []byte("deepseek/pro")) {
		t.Fatalf("magpie row missing: %q", out)
	}
	if bytes.Contains(out, []byte("not-the-official-list")) {
		t.Fatalf("merged a compressed body the IDE cannot parse: %q", out)
	}
	mu.Lock()
	gotEnc := enc
	mu.Unlock()
	if gotEnc != "identity" {
		t.Fatalf("upstream Accept-Encoding %q", gotEnc)
	}
}

func TestCompressedOfficialAppendIsForwarded(t *testing.T) {
	const conv = "11111111-1111-4111-8111-111111111111"
	plain := bidiAppend("other", runRequest(conv, "grok-4.7", nil))
	payload := plain[5:]
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 5+buf.Len())
	frame[0] = 1
	binary.BigEndian.PutUint32(frame[1:5], uint32(buf.Len()))
	copy(frame[5:], buf.Bytes())

	var got []byte
	var gwN int
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		gwN++
		http.Error(w, "gateway", http.StatusBadGateway)
	})
	up := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/proto")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ack"))
	})
	dir := t.TempDir()
	p := &Proxy{
		Addr: "127.0.0.1:0", CADir: dir, Upstream: up.URL,
		Gateway: func() (string, string) { return gw.URL, "tok" },
		Models:  func() []Model { return []Model{{ID: "deepseek/pro", Name: "DeepSeek Pro"}} },
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	client := cursorClient(t, strings.TrimPrefix(addr, "http://"), dir, "api2.cursor.sh")
	out := postCursor(t, client, "/aiserver.v1.BidiService/BidiAppend", frame)
	if !bytes.Equal(out, []byte("ack")) {
		t.Fatalf("response %q", out)
	}
	if !bytes.Equal(got, frame) {
		t.Fatalf("upstream len %d, want the compressed frame %d", len(got), len(frame))
	}
	if gwN != 0 {
		t.Fatalf("gateway was asked %d times", gwN)
	}
}
