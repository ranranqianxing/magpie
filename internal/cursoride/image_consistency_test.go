package cursoride

import (
	"bytes"
	"compress/gzip"
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

func tinyPNG() []byte {
	return []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x01, 0x02, 0x03, 0x04}
}

func tinyJPEG() []byte {
	return []byte{0xff, 0xd8, 0xff, 0x00, 0x11, 0x22, 0x33, 0x44}
}

func wrapB64(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i += 76 {
		if i > 0 {
			b.WriteByte('\n')
		}
		end := i + 76
		if end > len(s) {
			end = len(s)
		}
		b.WriteString(s[i:end])
	}
	return b.String()
}

func dataURLBytes(t *testing.T, body, mime string) []byte {
	t.Helper()
	prefix := "data:" + mime + ";base64,"
	i := strings.Index(body, prefix)
	if i < 0 {
		t.Fatalf("missing %s\n%s", prefix, body)
	}
	rest := body[i+len(prefix):]
	end := strings.IndexAny(rest, `"`)
	if end < 0 {
		t.Fatalf("data URL did not end: %s", rest[:min(80, len(rest))])
	}
	raw, err := base64.StdEncoding.DecodeString(rest[:end])
	if err != nil {
		t.Fatalf("data URL: %v", err)
	}
	return raw
}

func TestCompileKeepsBurstAndWrappedImages(t *testing.T) {
	png := append(tinyPNG(), bytes.Repeat([]byte{0x11}, 64<<10)...)
	jpeg := tinyJPEG()
	pngB64 := base64.StdEncoding.EncodeToString(png)
	jpegB64 := base64.StdEncoding.EncodeToString(jpeg)
	payload := invoke("inv-burst",
		userImage("what are these", wrapB64(pngB64), "image/png"),
		fileMsg("data:image/jpeg;base64,"+jpegB64, "image/jpeg"),
		fileMsg(pngB64, "image/png"),
	)
	// The second file part is another copy of the PNG. The first user
	// message carries the wrapped PNG. Both must decode to the picture,
	// and the JPEG file part must stay a data URL rather than a note.
	chat, err := CompileChat(Route{Model: "deepseek/deepseek-v4.1-flash"}, "conv", payload)
	if err != nil {
		t.Fatal(err)
	}
	body, err := chat.JSON()
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, "[attached file:") {
		t.Fatalf("a picture became a note:\n%s", text[:min(400, len(text))])
	}
	if got := dataURLBytes(t, text, "image/png"); !bytes.Equal(got, png) {
		t.Fatalf("png decoded %d want %d", len(got), len(png))
	}
	if got := dataURLBytes(t, text, "image/jpeg"); !bytes.Equal(got, jpeg) {
		t.Fatalf("jpeg decoded %d want %d", len(got), len(jpeg))
	}
	if strings.Count(text, "data:image/png;base64,") != 2 || strings.Count(text, "data:image/jpeg;base64,") != 1 {
		t.Fatalf("image count png=%d jpeg=%d", strings.Count(text, "data:image/png;base64,"), strings.Count(text, "data:image/jpeg;base64,"))
	}
}

func TestCompileDropsAPartialImageAndKeepsItsSibling(t *testing.T) {
	png := tinyPNG()
	var textInner wbuf
	textInner.str(1, "look")
	var textPart wbuf
	textPart.msg(1, textInner)
	var bad wbuf
	bad.msg(2, []byte{0x0a, 0x05, 0x01})
	var emptyImg wbuf
	emptyImg.str(1, "")
	emptyImg.str(2, "image/png")
	var emptyPart wbuf
	emptyPart.msg(2, emptyImg)
	var good wbuf
	good.str(1, base64.StdEncoding.EncodeToString(png))
	good.str(2, "image/png")
	var goodPart wbuf
	goodPart.msg(2, good)
	var parts wbuf
	parts.msg(1, textPart)
	parts.msg(1, goodPart)
	parts.msg(1, bad)
	parts.msg(1, emptyPart)
	var msg wbuf
	msg.u32(1, 1)
	msg.msg(3, parts)
	chat, err := CompileChat(Route{Model: "deepseek/deepseek-v4.1-flash"}, "conv", invoke("inv-part", msg))
	if err != nil {
		t.Fatal(err)
	}
	body, err := chat.JSON()
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Count(text, "data:image/png;base64,") != 1 {
		t.Fatalf("sibling image was not the only picture:\n%s", text)
	}
	if got := dataURLBytes(t, text, "image/png"); !bytes.Equal(got, png) {
		t.Fatalf("sibling png %d", len(got))
	}
	if !strings.Contains(text, "look") {
		t.Fatalf("text dropped:\n%s", text)
	}
	logged := Recent()
	if !strings.Contains(logged, "agent image skipped reason=partial source=part") || !strings.Contains(logged, "agent image skipped reason=empty source=part") {
		t.Fatalf("skip reasons:\n%s", logged)
	}
	if strings.Contains(logged, base64.StdEncoding.EncodeToString(png)) {
		t.Fatal("diagnostic kept the image")
	}
}

func TestCompileImageAndToolRoundKeepsEveryPicture(t *testing.T) {
	png := tinyPNG()
	jpeg := tinyJPEG()
	pngURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	payload := invoke("inv-mix",
		userImage("what is this", base64.StdEncoding.EncodeToString(png), "image/png"),
		assistantStructCall("reading", "call_1", "Read", "", map[string]any{"path": "a.go"}),
		toolResultValueMsg("call_1", []any{
			map[string]any{"type": "text", "text": "see"},
			map[string]any{"type": "image", "image": pngURL, "mediaType": "image/png"},
			map[string]any{"type": "image", "image": map[string]any{"data": base64.StdEncoding.EncodeToString(jpeg), "mediaType": "image/jpeg"}},
		}),
	)
	chat, err := CompileChat(Route{Model: "deepseek/deepseek-v4.1-flash"}, "conv", payload)
	if err != nil {
		t.Fatal(err)
	}
	body, err := chat.JSON()
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, `"path":"a.go"`) && !strings.Contains(text, `path\":\"a.go`) {
		t.Fatalf("read args dropped: %s", text)
	}
	if strings.Contains(text, `"arguments":"{}"`) {
		t.Fatalf("empty args: %s", text)
	}
	var got struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &got) != nil || len(got.Messages) != 3 {
		t.Fatalf("messages %s", text)
	}
	var user []struct {
		Type     string `json:"type"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if json.Unmarshal(got.Messages[0].Content, &user) != nil || len(user) != 2 || user[1].ImageURL.URL != pngURL {
		t.Fatalf("user image %#v", got.Messages[0].Content)
	}
	var tool []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if json.Unmarshal(got.Messages[2].Content, &tool) != nil || len(tool) != 3 || tool[0].Text != "see" || tool[1].ImageURL.URL != pngURL {
		t.Fatalf("tool images %#v", got.Messages[2].Content)
	}
	jpegPayload, ok := strings.CutPrefix(tool[2].ImageURL.URL, "data:image/jpeg;base64,")
	raw, decErr := base64.StdEncoding.DecodeString(jpegPayload)
	if !ok || decErr != nil || !bytes.Equal(raw, jpeg) {
		t.Fatalf("tool jpeg %q", tool[2].ImageURL.URL)
	}
	if !strings.Contains(Recent(), "agent image source=tool") || !strings.Contains(Recent(), "agent image source=part") {
		t.Fatalf("image trace:\n%s", Recent())
	}
}

func TestAppendReadLimitClearsACatalogSizedBody(t *testing.T) {
	if maxAppendBody <= maxCatalogBody {
		t.Fatal("append cap is not above the catalog cap")
	}
	got, err := readLimited(bytes.NewReader(make([]byte, maxCatalogBody+1)), maxAppendBody)
	if err != nil || len(got) != maxCatalogBody+1 {
		t.Fatalf("catalog-sized append was refused: %v len=%d", err, len(got))
	}
	if _, err := readLimited(bytes.NewReader(make([]byte, 8)), 4); err == nil {
		t.Fatal("past the cap was accepted")
	}
	raw := bytes.Repeat([]byte{0}, maxCatalogBody+1)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	plain, ok := gunzipRequestLimit(buf.Bytes(), "gzip", maxAppendBody)
	if !ok || len(plain) != len(raw) {
		t.Fatalf("gunzip ok=%v len=%d want %d", ok, len(plain), len(raw))
	}
	if _, ok := gunzipRequestLimit(buf.Bytes(), "gzip", maxCatalogBody); ok {
		t.Fatal("catalog cap accepted a larger gzip body")
	}
}

func TestBidiAppendSkippedReadStillAnswersWithTheImage(t *testing.T) {
	t.Setenv("MAGPIE_DEBUG", "1")
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	const conv = "11111111-1111-4111-8111-111111111111"
	const model = "deepseek/deepseek-v4.1-flash"
	png := tinyPNG()
	pngURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	var bodies [][]byte
	gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "text/event-stream")
		if len(bodies) == 1 {
			io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_r","function":{"name":"Read","arguments":"{}"}}]}}]}`+"\n\n")
			io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
			io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"it is a picture"}}]}`+"\n\n")
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
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
	payload := invoke("inv-img", userImage("what is this", base64.StdEncoding.EncodeToString(png), "image/png"))
	stream := inferenceInvokeStream(t, client, conv, model, payload)
	if strings.Contains(stream, "call_r") {
		t.Fatalf("empty Read was sent: %s", stream)
	}
	if !strings.Contains(stream, "it is a picture") {
		t.Fatalf("description missing: %s", stream)
	}
	if len(bodies) != 2 {
		t.Fatalf("gateway rounds %d", len(bodies))
	}
	second := string(bodies[1])
	if !strings.Contains(second, pngURL) || !strings.Contains(second, "missing path") {
		t.Fatalf("second chat lost the image or the error: %s", second)
	}
	if strings.Contains(second, `"arguments":"{}"`) && !strings.Contains(second, "missing path") {
		t.Fatalf("retry dropped the skip: %s", second)
	}
	logged := buf.String()
	if strings.Contains(logged, pngURL) || strings.Contains(logged, base64.StdEncoding.EncodeToString(png)) {
		t.Fatalf("diagnostic kept the image:\n%s", logged)
	}
	if !strings.Contains(logged, "chat start model="+model+" path=BidiAppend history=inline") || !strings.Contains(logged, "images=1") || !strings.Contains(logged, "empty-args=0") || !strings.Contains(logged, "image-bytes=") {
		t.Fatalf("chat start:\n%s", logged)
	}
	if !strings.Contains(logged, "agent tool round runnable=0 skipped=1 images=1") || !strings.Contains(logged, "agent image source=part") {
		t.Fatalf("round trace:\n%s", logged)
	}
	if strings.Contains(logged, "path=AgentService/Run") || strings.Contains(logged, "agent kv hydrate") {
		t.Fatalf("this invoke took the Run path:\n%s", logged)
	}
}

func TestBidiAppendLargerThanACatalogBodyIsDecoded(t *testing.T) {
	up := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	dir := t.TempDir()
	p := &Proxy{
		Addr: "127.0.0.1:0", CADir: dir, Upstream: up.URL,
		Gateway: func() (string, string) { return "http://127.0.0.1:1", "" },
		Models:  func() []Model { return nil },
	}
	addr, err := p.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	client := cursorClient(t, strings.TrimPrefix(addr, "http://"), dir, "api2.cursor.sh")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = postCursorContext(ctx, client, "/aiserver.v1.BidiService/BidiAppend", make([]byte, maxCatalogBody+1))
	if err == nil {
		t.Fatal("oversized append was accepted as a catalog")
	}
	if strings.Contains(err.Error(), `"read`) {
		t.Fatalf("append over the catalog cap was refused before decode: %v", err)
	}
	if !strings.Contains(err.Error(), "append") {
		t.Fatalf("decode was not attempted: %v", err)
	}
}

func inferenceInvokeStream(t *testing.T, client *http.Client, conv, model string, payload []byte) string {
	t.Helper()
	const reqID = "req-image"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
	return stream.String()
}
