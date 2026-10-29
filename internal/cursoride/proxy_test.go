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
