package cursoride

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func gzipBody(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func postCursorHeaders(t *testing.T, c *http.Client, path string, body []byte, hdr map[string]string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api2.cursor.sh"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/proto")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: %s %q", path, resp.Status, got)
	}
	return got
}

type captured struct {
	mu   sync.Mutex
	enc  string
	n    int64
	body []byte
	path string
}

func (c *captured) save(r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enc = r.Header.Get("Content-Encoding")
	c.n = r.ContentLength
	c.body = append([]byte(nil), b...)
	c.path = r.URL.Path
}

func (c *captured) get() (enc string, n int64, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enc, c.n, append([]byte(nil), c.body...)
}

// A BidiAppend Cursor gzipped is inspected decompressed, then forwarded
// as that plain body. Leaving Content-Encoding: gzip on it is the
// upstream "decompression failed" that stalls the stream.
func TestProxyForwardsGunzippedBidiWithMatchingHeaders(t *testing.T) {
	const conv = "11111111-1111-4111-8111-111111111111"
	plain := bidiAppend("other", runRequest(conv, "composer-1", nil))
	gz := gzipBody(t, plain)
	cases := []struct {
		name string
		body []byte
		enc  string
	}{
		{"gzip", gz, "gzip"},
		{"x-gzip", gz, "x-gzip"},
		{"magic only", gz, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got captured
			up := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				got.save(r)
				w.Header().Set("Content-Type", "application/proto")
				w.WriteHeader(http.StatusOK)
			})
			dir := t.TempDir()
			p := &Proxy{Addr: "127.0.0.1:0", CADir: dir, Upstream: up.URL, Gateway: func() (string, string) { return "", "" }}
			addr, err := p.Start()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { p.Close() })
			client := cursorClient(t, strings.TrimPrefix(addr, "http://"), dir, "api2.cursor.sh")
			hdr := map[string]string{}
			if tc.enc != "" {
				hdr["Content-Encoding"] = tc.enc
			}
			postCursorHeaders(t, client, "/aiserver.v1.BidiService/BidiAppend", tc.body, hdr)
			enc, n, body := got.get()
			if enc != "" || n != int64(len(plain)) || !bytes.Equal(body, plain) {
				t.Fatalf("upstream encoding %q length %d body %d, want plain %d", enc, n, len(body), len(plain))
			}
		})
	}
}

// A path this package does not gunzip keeps the gzip body and the header
// together. Catalog requests are forwarded to Cursor as the client sent
// them; only the reply is decompressed for the merge.
func TestProxyKeepsGzipItDidNotDecompress(t *testing.T) {
	plain := []byte("catalog-request-bytes")
	gz := gzipBody(t, plain)
	var got captured
	up := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		got.save(r)
		w.Header().Set("Content-Type", "application/proto")
		w.Write(AvailableModels(nil))
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
	out := postCursorHeaders(t, client, PathAvailableModels, gz, map[string]string{"Content-Encoding": "gzip"})
	enc, n, body := got.get()
	if enc != "gzip" || n != int64(len(gz)) || !bytes.Equal(body, gz) {
		t.Fatalf("catalog upstream encoding %q length %d (gzip %d)", enc, n, len(gz))
	}
	if !bytes.Contains(out, []byte("deepseek/pro")) {
		t.Fatalf("merged catalog %q", out)
	}

	var other captured
	up2 := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		other.save(r)
		w.WriteHeader(http.StatusOK)
	})
	p.Upstream = up2.URL
	postCursorHeaders(t, client, "/aiserver.v1.AiService/NotCatalog", gz, map[string]string{"Content-Encoding": "gzip"})
	enc, n, body = other.get()
	if enc != "gzip" || n != int64(len(gz)) || !bytes.Equal(body, gz) {
		t.Fatalf("forwarded encoding %q length %d", enc, n)
	}
}

// A gzipped BidiAppend for a catalog model still stays local and the
// gateway text stream is what Cursor's inference stream completes with.
func TestProxyLocalChatCompletesWhenBidiIsGzip(t *testing.T) {
	const token = "gateway-token-gzip"
	const conv = "11111111-1111-4111-8111-111111111111"
	gotChat := make(chan []byte, 1)
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		gotChat <- b
	})
	var forwarded int
	up := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		forwarded++
		http.Error(w, "nope", http.StatusBadRequest)
	})
	dir := t.TempDir()
	p := &Proxy{
		Addr: "127.0.0.1:0", CADir: dir, Upstream: up.URL,
		Gateway: func() (string, string) { return gw.URL, token },
		Models: func() []Model {
			return []Model{{ID: "deepseek/pro", Name: "DeepSeek Pro"}}
		},
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	client := cursorClient(t, strings.TrimPrefix(addr, "http://"), dir, "api2.cursor.sh")
	reqID := "req-gz"
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
	gzPost := func(payload []byte) {
		t.Helper()
		postCursorHeaders(t, client, "/aiserver.v1.BidiService/BidiAppend", gzipBody(t, payload), map[string]string{"Content-Encoding": "gzip"})
	}
	gzPost(bidiAppend(reqID, runRequest(conv, "deepseek/pro", nil)))
	gzPost(bidiAppend(reqID, invoke("inv-1", textMsg(1, "hi"))))
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
		if !finished && bytes.Contains(stream.Bytes(), []byte("Hello")) {
			finished = true
			gzPost(bidiAppend(reqID, finishMsg()))
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
	if !bytes.Contains(stream.Bytes(), []byte("Hello")) {
		t.Fatalf("stream = %q", stream.Bytes())
	}
	select {
	case chat := <-gotChat:
		if !bytes.Contains(chat, []byte("deepseek/pro")) {
			t.Fatalf("chat %s", chat)
		}
	default:
		t.Fatal("gateway was not asked")
	}
	if forwarded != 0 {
		t.Fatalf("local run was forwarded %d times", forwarded)
	}
}

func TestGunzipRequestLeavesAPlainBodyAlone(t *testing.T) {
	plain := []byte{0x0a, 0x02, 'h', 'i'}
	out, changed := gunzipRequest(plain, "")
	if changed || !bytes.Equal(out, plain) {
		t.Fatalf("changed %v %q", changed, out)
	}
	gz := gzipBody(t, plain)
	out, changed = gunzipRequest(gz, "gzip")
	if !changed || !bytes.Equal(out, plain) {
		t.Fatalf("gzip changed %v %q", changed, out)
	}
	out, changed = gunzipRequest([]byte{0x1f, 0x8b, 0x00}, "gzip")
	if changed || !bytes.Equal(out, []byte{0x1f, 0x8b, 0x00}) {
		t.Fatalf("bad gzip changed %v", changed)
	}
}
