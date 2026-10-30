package cursoride

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProxyInjectsTheCatalogAndStreamsAMagpieModel(t *testing.T) {
	const token = "gateway-token-for-cursor-ide"
	const conv = "11111111-1111-4111-8111-111111111111"
	gotChat := make(chan []byte, 1)
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("authorization %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-Magpie-Session") != conv {
			t.Errorf("session %q", r.Header.Get("X-Magpie-Session"))
		}
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"ponder\",\"content\":\"Hello\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"!\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		gotChat <- b
	})
	var forwarded []string
	up := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		forwarded = append(forwarded, r.URL.Path)
		w.Header().Set("Content-Type", "application/proto")
		w.WriteHeader(http.StatusOK)
	})
	dir := t.TempDir()
	p := &Proxy{
		Addr:     "127.0.0.1:0",
		CADir:    dir,
		Upstream: up.URL,
		Gateway:  func() (string, string) { return gw.URL, token },
		Models: func() []Model {
			return []Model{{ID: "deepseek/pro", Name: "DeepSeek Pro"}}
		},
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	host := strings.TrimPrefix(addr, "http://")
	client := cursorClient(t, host, dir, "api2.cursor.sh")

	body := postCursor(t, client, PathAvailableModels, frameOf(nil))
	if !bytes.Contains(body, []byte("DeepSeek Pro")) || !bytes.Contains(body, []byte("deepseek/pro")) {
		t.Fatalf("catalog = %q", body)
	}
	if bytes.Contains(body, []byte(token)) {
		t.Fatal("catalog contains the gateway token")
	}
	again := postCursor(t, client, PathAvailableModels, frameOf(nil))
	if !bytes.Contains(again, []byte("deepseek/pro")) {
		t.Fatal("second catalog request failed")
	}

	reqID := "req-1"
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
	postCursor(t, client, "/aiserver.v1.BidiService/BidiAppend", bidiAppend(reqID, runRequest(conv, "deepseek/pro", nil)))
	postCursor(t, client, "/aiserver.v1.BidiService/BidiAppend", bidiAppend(reqID, invoke("inv-1", textMsg(1, "hi"))))
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
	tmp := make([]byte, 512)
	finished := false
	for {
		n, rerr := resp.Body.Read(tmp)
		stream.Write(tmp[:n])
		if !finished && bytes.Contains(stream.Bytes(), []byte("Hello")) && bytes.Contains(stream.Bytes(), []byte("!")) {
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
	b := stream.Bytes()
	if !bytes.Contains(b, []byte("Hello")) || !bytes.Contains(b, []byte("ponder")) || !bytes.Contains(b, []byte("!")) {
		t.Fatalf("stream = %q", b)
	}
	if !bytes.Contains(b, []byte{0x02, 0, 0, 0, 0x02, '{', '}'}) {
		t.Fatal("stream has no end frame")
	}
	if bytes.Contains(b, []byte(token)) {
		t.Fatal("stream contains the gateway token")
	}
	select {
	case chat := <-gotChat:
		if bytes.Contains(chat, []byte(token)) {
			t.Fatalf("chat body contains the token: %s", chat)
		}
		var got struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if json.Unmarshal(chat, &got) != nil || got.Model != "deepseek/pro" || !got.Stream {
			t.Fatalf("chat %s", chat)
		}
	default:
		t.Fatal("gateway was not asked")
	}

	postCursor(t, client, "/aiserver.v1.BidiService/BidiAppend", bidiAppend("other", runRequest(conv, "composer-1", nil)))
	if !strings.Contains(strings.Join(forwarded, " "), "/aiserver.v1.BidiService/BidiAppend") {
		t.Fatalf("Cursor's own model was not forwarded: %v", forwarded)
	}
}

func TestProxyAgentRunAnswersCatalogModelsLocally(t *testing.T) {
	const token = "gateway-token-for-cursor-ide"
	const conv = "22222222-2222-4222-8222-222222222222"
	gotChat := make(chan []byte, 4)
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("authorization %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-Magpie-Session") != conv {
			t.Errorf("session %q", r.Header.Get("X-Magpie-Session"))
		}
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"ponder\",\"content\":\"Hello\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		gotChat <- b
	})
	var forwarded []string
	up := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		forwarded = append(forwarded, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	})
	dir := t.TempDir()
	p := &Proxy{
		Addr:     "127.0.0.1:0",
		CADir:    dir,
		Upstream: up.URL,
		Gateway:  func() (string, string) { return gw.URL, token },
		Models:   func() []Model { return agentCatalog() },
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	client := cursorClient(t, strings.TrimPrefix(addr, "http://"), dir, "api2.cursor.sh")

	ids := []string{
		"aicodings-openai/gpt-6-astra",
		"codex/gpt-5.6-luna",
		"workbuddy/deepseek-v4.1-flash",
	}
	for _, id := range ids {
		var params []Parameter
		if id == "aicodings-openai/gpt-6-astra" {
			params = []Parameter{{ID: "reasoning", Value: "high"}}
		}
		body := postCursor(t, client, PathAgentRun, agentRunBody(id, conv, "你好", params))
		if !bytes.Contains(body, []byte("Hello")) || !bytes.Contains(body, []byte("ponder")) {
			t.Fatalf("%s was not answered from the gateway (stream %q, forwarded %v)", id, body, forwarded)
		}
		if bytes.Contains(body, []byte(token)) {
			t.Fatalf("%s stream contains the gateway token", id)
		}
		if !bytes.Contains(body, []byte{0x02, 0, 0, 0, 0x02, '{', '}'}) {
			t.Fatalf("%s stream has no end frame", id)
		}
	}
	if len(forwarded) != 0 {
		t.Fatalf("catalog models were forwarded: %v", forwarded)
	}
	for i, id := range ids {
		select {
		case chat := <-gotChat:
			if bytes.Contains(chat, []byte(token)) {
				t.Fatalf("chat body contains the token: %s", chat)
			}
			var got struct {
				Model    string `json:"model"`
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
				Effort string `json:"reasoning_effort"`
				Stream bool   `json:"stream"`
			}
			if json.Unmarshal(chat, &got) != nil || got.Model != id || !got.Stream {
				t.Fatalf("chat %d %s", i, chat)
			}
			if len(got.Messages) != 1 || got.Messages[0].Content != "你好" {
				t.Fatalf("messages %s", chat)
			}
			if id == "aicodings-openai/gpt-6-astra" && got.Effort != "high" {
				t.Fatalf("effort %q", got.Effort)
			}
			if id != "aicodings-openai/gpt-6-astra" && got.Effort != "" {
				t.Fatalf("effort %q on %s", got.Effort, id)
			}
		default:
			t.Fatalf("gateway was not asked for %s", id)
		}
	}

	empty := postCursor(t, client, PathAgentRun, agentRunBody("codex/gpt-5.6-luna", conv, "  ", nil))
	if !bytes.Contains(empty, []byte("cursor ide: agent run has no message")) {
		t.Fatalf("empty run = %q", empty)
	}
	if len(forwarded) != 0 {
		t.Fatalf("empty catalog run was forwarded: %v", forwarded)
	}

	postCursor(t, client, PathAgentRun, agentRunBody("grok-4.7", conv, "hi", nil))
	if !strings.Contains(strings.Join(forwarded, " "), PathAgentRun) {
		t.Fatalf("Cursor's own model was not forwarded: %v", forwarded)
	}
	logText := Recent()
	for _, id := range ids {
		if !strings.Contains(logText, "route ok model="+id) || !strings.Contains(logText, "chat start model="+id) || !strings.Contains(logText, "chat done model="+id) {
			t.Fatalf("log missing %s:\n%s", id, logText)
		}
	}
	if !strings.Contains(logText, "route miss model=grok-4.7") || strings.Contains(logText, "Unknown model ID") {
		t.Fatalf("log should name the forward without claiming Unknown model ID:\n%s", logText)
	}
	if strings.Contains(logText, "你好") || strings.Contains(logText, token) {
		t.Fatalf("log kept the user text or the gateway token:\n%s", logText)
	}
}

func TestProxyTunnelsWhatIsNotCursor(t *testing.T) {
	hit := false
	up := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		hit = true
		if r.URL.Path != "/elsewhere" {
			t.Errorf("path %s", r.URL.Path)
		}
		w.Write([]byte("ok-tunneled"))
	})
	dir := t.TempDir()
	p := &Proxy{Addr: "127.0.0.1:0", CADir: dir, Gateway: func() (string, string) { return "", "" }}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	proxyAddr := strings.TrimPrefix(addr, "http://")
	target := strings.TrimPrefix(up.URL, "http://")
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("connect %s", resp.Status)
	}
	if _, err := fmt.Fprintf(c, "GET /elsewhere HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", target); err != nil {
		t.Fatal(err)
	}
	got, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(got.Body)
	if !hit || string(b) != "ok-tunneled" {
		t.Fatalf("hit %v body %q", hit, b)
	}
}

func TestCAIsReusedAndAMismatchedKeyIsKept(t *testing.T) {
	dir := t.TempDir()
	cert, _, err := EnsureCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := EnsureCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cert.Equal(again) {
		t.Fatal("a second EnsureCA wrote a new CA")
	}
	other, key, err := EnsureCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePEM(dir+"/ca.key", "PRIVATE KEY", keyDER, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := parseCA(mustRead(t, CertFile(dir)), mustRead(t, dir+"/ca.key")); err == nil {
		t.Fatal("a key from another CA was accepted")
	}
	if !other.IsCA {
		t.Fatal("other CA")
	}
	os.Remove(dir + "/ca.key")
	if _, _, err := EnsureCA(dir); err == nil {
		t.Fatal("a certificate without its key was replaced")
	}
}

func TestEnsureCAReplacesAnExpiredCertificate(t *testing.T) {
	dir := t.TempDir()
	old, _, err := mintCA(dir, time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if certUsable(old) {
		t.Fatal("fixture certificate is still valid")
	}
	fresh, _, err := EnsureCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !certUsable(fresh) || fresh.Equal(old) {
		t.Fatal("an expired CA was kept")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type localServer struct{ URL string }

func localHTTP(t *testing.T, h http.HandlerFunc) *localServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return &localServer{URL: "http://" + ln.Addr().String()}
}

func cursorClient(t *testing.T, proxyAddr, caDir, host string) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(mustRead(t, CertFile(caDir))) {
		t.Fatal("ca pem")
	}
	tr := &http.Transport{
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			c, err := d.DialContext(ctx, "tcp", proxyAddr)
			if err != nil {
				return nil, err
			}
			if _, err := fmt.Fprintf(c, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n", host, host); err != nil {
				c.Close()
				return nil, err
			}
			br := bufio.NewReader(c)
			resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
			if err != nil {
				c.Close()
				return nil, err
			}
			if resp.StatusCode != http.StatusOK {
				c.Close()
				return nil, fmt.Errorf("connect %s", resp.Status)
			}
			if br.Buffered() > 0 {
				c.Close()
				return nil, fmt.Errorf("proxy buffered %d bytes", br.Buffered())
			}
			tlsConn := tls.Client(c, &tls.Config{ServerName: host, RootCAs: pool})
			if err := tlsConn.HandshakeContext(ctx); err != nil {
				c.Close()
				return nil, err
			}
			return tlsConn, nil
		},
	}
	return &http.Client{Transport: tr}
}

func postCursor(t *testing.T, c *http.Client, path string, body []byte) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := postCursorContext(ctx, c, path, body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func postCursorContext(ctx context.Context, c *http.Client, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api2.cursor.sh"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/proto")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s %q", path, resp.Status, b)
	}
	return b, nil
}

func sseRequest(id string) []byte {
	var msg wbuf
	msg.str(1, id)
	return frameOf(msg)
}

func bidiAppend(id string, payload []byte) []byte {
	var rid wbuf
	rid.str(1, id)
	var msg wbuf
	msg.str(1, hex.EncodeToString(payload))
	msg.msg(2, rid)
	return frameOf(msg)
}

func bidiAppendBinary(id string, payload []byte) []byte {
	var rid wbuf
	rid.str(1, id)
	var msg wbuf
	msg.msg(4, payload)
	msg.msg(2, rid)
	return frameOf(msg)
}

func finishMsg() []byte {
	var client wbuf
	client.msg(4, nil)
	return []byte(client)
}

func TestProxyDataBinaryBidiAppend(t *testing.T) {
	const token = "gateway-token-bin"
	const conv = "11111111-1111-4111-8111-111111111111"
	gotChat := make(chan []byte, 1)
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		gotChat <- b
	})
	up := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadRequest)
	})
	dir := t.TempDir()
	p := &Proxy{
		Addr: "127.0.0.1:0", CADir: dir, Upstream: up.URL,
		Gateway: func() (string, string) { return gw.URL, token },
		Models: func() []Model {
			return []Model{{ID: "deepseek/deepseek-flash", Name: "Flash"}}
		},
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	host := strings.TrimPrefix(addr, "http://")
	client := cursorClient(t, host, dir, "api2.cursor.sh")

	reqID := "req-bin"
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
	postCursor(t, client, "/aiserver.v1.BidiService/BidiAppend", bidiAppendBinary(reqID, runRequest(conv, "deepseek/deepseek-flash", nil)))
	postCursor(t, client, "/aiserver.v1.BidiService/BidiAppend", bidiAppendBinary(reqID, invoke("inv-1", textMsg(1, "hi"))))
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
	tmp := make([]byte, 512)
	finished := false
	for {
		n, rerr := resp.Body.Read(tmp)
		stream.Write(tmp[:n])
		if !finished && bytes.Contains(stream.Bytes(), []byte("OK")) {
			finished = true
			postCursor(t, client, "/aiserver.v1.BidiService/BidiAppend", bidiAppendBinary(reqID, finishMsg()))
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
	if !bytes.Contains(stream.Bytes(), []byte("OK")) {
		t.Fatalf("stream = %q", stream.Bytes())
	}
	select {
	case <-gotChat:
	default:
		t.Fatal("gateway was not asked — data_binary path likely forwarded upstream")
	}
}

func TestProxyAgentRunReplaysHistoryAndEmitsShell(t *testing.T) {
	const conv = "33333333-3333-4333-8333-333333333333"
	gotChat := make(chan []byte, 1)
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Shell","arguments":"{\"command\":\"ls\"}"}}]}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
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
		Models:  func() []Model { return agentCatalog() },
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	client := cursorClient(t, strings.TrimPrefix(addr, "http://"), dir, "api2.cursor.sh")
	body := postCursor(t, client, PathAgentRun, agentRunWithHistory("codex/gpt-5.6-luna", conv, "second"))
	if !bytes.Contains(body, []byte("call_1")) || !agentExecRan(body, "ls") {
		t.Fatalf("stream did not exec the shell: %q", body)
	}
	select {
	case chat := <-gotChat:
		var got struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if json.Unmarshal(chat, &got) != nil {
			t.Fatalf("chat %s", chat)
		}
		if len(got.Messages) != 3 || got.Messages[0].Content == "" || !strings.Contains(got.Messages[0].Content, "first") || got.Messages[1].Role != "assistant" || got.Messages[1].Content != "earlier" || got.Messages[2].Content != "second" {
			t.Fatalf("messages %+v", got.Messages)
		}
		var shell bool
		for _, tool := range got.Tools {
			if tool.Function.Name == "Shell" {
				shell = true
			}
		}
		if !shell {
			t.Fatal("agent chat offered no Shell tool")
		}
	default:
		t.Fatal("gateway was not asked")
	}
}

func TestProxyAgentRunClosesAToolCallWhenTheStreamEnds(t *testing.T) {
	gotChat := make(chan struct{}, 1)
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_2","function":{"name":"Shell","arguments":"{\"command\":\"pwd\"}"}}]}}]}`+"\n\n")
		gotChat <- struct{}{}
	})
	dir := t.TempDir()
	p := &Proxy{
		Addr: "127.0.0.1:0", CADir: dir,
		Upstream: localHTTP(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }).URL,
		Gateway:  func() (string, string) { return gw.URL, "tok" },
		Models:   func() []Model { return agentCatalog() },
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	client := cursorClient(t, strings.TrimPrefix(addr, "http://"), dir, "api2.cursor.sh")
	body := postCursor(t, client, PathAgentRun, agentRunWithHistory("codex/gpt-5.6-luna", "55555555-5555-4555-8555-555555555555", "second"))
	select {
	case <-gotChat:
	default:
		t.Fatal("gateway was not asked")
	}
	if !agentExecRan(body, "pwd") {
		t.Fatalf("stream did not exec the shell: %q", body)
	}
}

// agentExecRan reports whether body carries an exec_server_message whose
// shell command is cmd. A partial_tool_call names the command too; this
// looks only at the exec the IDE runs.
func agentExecRan(body []byte, cmd string) bool {
	payloads, framed := connectPayloads(body)
	if !framed {
		return false
	}
	for _, p := range payloads {
		fs, err := parseFields(p)
		if err != nil {
			continue
		}
		execs := fieldBytes(fs, 2)
		if len(execs) == 0 {
			continue
		}
		msg, err := parseFields(execs[0])
		if err != nil {
			continue
		}
		shells := fieldBytes(msg, 46)
		if len(shells) == 0 {
			continue
		}
		shell, err := parseFields(shells[0])
		if err != nil {
			continue
		}
		if got, _ := fieldString(shell, 1); got == cmd {
			return true
		}
	}
	return false
}

func TestProxyInferenceEmitsToolCall(t *testing.T) {
	const conv = "44444444-4444-4444-8444-444444444444"
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_9","function":{"name":"Read","arguments":"{\"path\":\"a.go\"}"}}]}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	dir := t.TempDir()
	p := &Proxy{
		Addr: "127.0.0.1:0", CADir: dir, Upstream: localHTTP(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }).URL,
		Gateway: func() (string, string) { return gw.URL, "tok" },
		Models:  func() []Model { return []Model{{ID: "deepseek/pro", Name: "DeepSeek Pro"}} },
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	client := cursorClient(t, strings.TrimPrefix(addr, "http://"), dir, "api2.cursor.sh")
	reqID := "req-tool"
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
	postCursor(t, client, "/aiserver.v1.BidiService/BidiAppend", bidiAppend(reqID, runRequest(conv, "deepseek/pro", nil)))
	postCursor(t, client, "/aiserver.v1.BidiService/BidiAppend", bidiAppend(reqID, invoke("inv-1", textMsg(1, "hi"))))
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
	tmp := make([]byte, 512)
	finished := false
	for {
		n, rerr := resp.Body.Read(tmp)
		stream.Write(tmp[:n])
		if !finished && bytes.Contains(stream.Bytes(), []byte("a.go")) {
			finished = true
			postCursor(t, client, "/aiserver.v1.BidiService/BidiAppend", bidiAppend(reqID, finishMsg()))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if ctx.Err() != nil {
				t.Fatalf("stream ended without the tool call: %q (%v)", stream.Bytes(), ctx.Err())
			}
			t.Fatalf("read: %v; stream=%q", rerr, stream.Bytes())
		}
		if ctx.Err() != nil {
			t.Fatalf("stream ended without the tool call: %q (%v)", stream.Bytes(), ctx.Err())
		}
	}
	b := stream.Bytes()
	if !bytes.Contains(b, []byte("call_9")) || !bytes.Contains(b, []byte("Read")) || !bytes.Contains(b, []byte("a.go")) {
		t.Fatalf("stream = %q", b)
	}
}

func TestProxyAgentRunLoopsWhenToolResultsAreBuffered(t *testing.T) {
	const conv = "77777777-7777-4777-8777-777777777777"
	var mu sync.Mutex
	var bodies [][]byte
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
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
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"listed"}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	dir := t.TempDir()
	p := &Proxy{
		Addr: "127.0.0.1:0", CADir: dir,
		Upstream: localHTTP(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }).URL,
		Gateway:  func() (string, string) { return gw.URL, "tok" },
		Models:   func() []Model { return agentCatalog() },
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	client := cursorClient(t, strings.TrimPrefix(addr, "http://"), dir, "api2.cursor.sh")
	run := frameOf(agentClient(agentRunMessage("codex/gpt-5.6-luna", conv, "go", nil)))
	tool := frameOf(agentToolResultClient("call_1", "listed"))
	body := append(append([]byte(nil), run...), tool...)
	out := postCursor(t, client, PathAgentRun, body)
	if !agentExecRan(out, "ls") || !bytes.Contains(out, []byte("listed")) {
		t.Fatalf("stream %q", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("gateway rounds %d", len(bodies))
	}
}

func agentRunWithHistory(model, conv, text string) []byte {
	user := []byte(`{"role":"user","content":"<user_query>\nfirst\n</user_query>"}`)
	asst := []byte(`{"role":"assistant","content":"earlier"}`)
	var state wbuf
	state.msg(1, user)
	state.msg(1, asst)
	var requested wbuf
	requested.str(1, model)
	var msg wbuf
	msg.str(1, text)
	var action wbuf
	action.msg(1, []byte(msg))
	var uma wbuf
	uma.msg(1, []byte(action))
	var run wbuf
	run.msg(1, []byte(state))
	run.msg(2, []byte(uma))
	run.str(5, conv)
	run.msg(9, []byte(requested))
	return frameOf(agentClient([]byte(run)))
}
