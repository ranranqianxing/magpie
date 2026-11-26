package cursoride

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ListenAddr is the proxy Cursor's http.proxy is pointed at. It listens on
// loopback only. Cursor's own hosts are the only ones whose TLS it terminates.
const ListenAddr = "127.0.0.1:47331"

// ProxyURL is that address as Cursor's settings store it.
func ProxyURL() string { return "http://" + ListenAddr }

// Proxy is the local proxy ordinary Cursor is pointed at while the Cursor
// agent is connected. It is not mounted on the gateway listener.
//
// Gateway is called on each local run for the gateway's root URL and the
// bearer token. Models is the catalog to inject, read on each catalog
// request. CADir holds the CA files. Upstream replaces Cursor's origin in
// tests; empty means the host Cursor asked for.
type Proxy struct {
	Addr     string
	CADir    string
	Gateway  func() (baseURL, token string)
	Models   func() []Model
	Upstream string

	mu     sync.Mutex
	ln     net.Listener
	srv    *http.Server
	ca     *x509.Certificate
	key    crypto.Signer
	leaves map[string]*tls.Certificate
	hub    *hub
	client *http.Client
}

// Start listens. A second Start returns the address already bound.
func (p *Proxy) Start() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		return "http://" + p.ln.Addr().String(), nil
	}
	if p.Models == nil {
		p.Models = func() []Model { return nil }
	}
	if p.Gateway == nil {
		p.Gateway = func() (string, string) { return "", "" }
	}
	ca, key, err := EnsureCA(p.CADir)
	if err != nil {
		return "", err
	}
	p.ca, p.key = ca, key
	p.leaves = map[string]*tls.Certificate{}
	if p.hub != nil {
		p.hub.stopAll()
	}
	p.hub = newHub()
	p.client = &http.Client{Transport: upstreamTransport()}
	addr := p.Addr
	if addr == "" {
		addr = ListenAddr
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	p.ln = ln
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	p.srv = srv
	// Serve the local server. Reading p.srv here races Close, which
	// clears that field while this goroutine is still starting.
	go func() { _ = srv.Serve(ln) }()
	return "http://" + ln.Addr().String(), nil
}

// Close stops listening.
func (p *Proxy) Close() error {
	p.mu.Lock()
	srv := p.srv
	h := p.hub
	p.ln, p.srv = nil, nil
	p.mu.Unlock()
	// In-flight gateway calls use the session context, not the hijacked
	// CONNECT, so shutting the listener down does not stop them.
	if h != nil {
		h.stopAll()
	}
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

// ServeHTTP is the proxy port. CONNECT to a Cursor host is terminated.
// Any other CONNECT is tunneled, so the rest of Cursor's traffic still flows.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "connect", http.StatusMethodNotAllowed)
		return
	}
	host := hostOnly(r.Host)
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "connect", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		client.Close()
		return
	}
	if !isCursorHost(host) {
		tunnel(client, r.Host)
		return
	}
	noteTLS(host)
	leaf, err := p.leaf(host)
	if err != nil {
		client.Close()
		return
	}
	tlsConn := tls.Server(client, &tls.Config{Certificates: []tls.Certificate{*leaf}})
	inner := &http.Server{Handler: http.HandlerFunc(p.mitm), ReadHeaderTimeout: 30 * time.Second}
	// Serve's listener Close runs when Accept ends. The second Accept
	// waits until this connection closes, so that Close does not drop a
	// keep-alive session Cursor still has requests on.
	_ = inner.Serve(newSingleListener(tlsConn))
}

func (p *Proxy) leaf(host string) (*tls.Certificate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c := p.leaves[host]; c != nil {
		return c, nil
	}
	c, err := leaf(p.ca, p.key, host)
	if err != nil {
		return nil, err
	}
	p.leaves[host] = c
	return c, nil
}

func (p *Proxy) mitm(w http.ResponseWriter, r *http.Request) {
	Note(fmt.Sprintf("proxy %s %s models=%d", r.Method, r.URL.Path, len(p.Models())))
	switch r.URL.Path {
	case PathAvailableModels, PathUsableModels, PathUsableModelsAI, PathDefaultModel, PathDefaultModelCLI, PathDefaultModelNudge:
		(&Handler{Models: p.Models, Upstream: p.fetch}).ServeHTTP(w, r)
	case "/aiserver.v1.NetworkService/IsConnected":
		writeProto(w, nil, "application/proto")
	case "/aiserver.v1.InferenceService/RunInferenceSSE":
		p.inferenceSSE(w, r)
	case "/aiserver.v1.BidiService/BidiAppend":
		p.bidi(w, r)
	case PathAgentRun:
		p.agentRun(w, r)
	default:
		// RunSSE is AgentService/Run once disableHttp2 has put the IDE
		// on HTTP/1.1. Its body is a request id; the model id rides on
		// BidiAppend or on Run. A catalog id is compiled on those. This
		// call, and every other path, is Cursor's own and is streamed
		// through unchanged.
		p.forward(w, r, nil, false)
	}
}

func (p *Proxy) inferenceSSE(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(body) > 1<<20 {
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	id, err := sseRequestID(body)
	if err != nil || id == "" {
		http.Error(w, "request", http.StatusBadRequest)
		return
	}
	s := p.hub.sess(id)
	if !s.wait(r.Context()) {
		return
	}
	if !s.isLocal() {
		p.forward(w, r.WithContext(r.Context()), body, false)
		return
	}
	// Leaving this stream does not cancel the gateway call. Cursor can
	// open RunInferenceSSE again for the same request id and keep reading.
	// A second reader replaces this one; the frames stay queued.
	watch, detach := s.attach()
	defer detach()
	w.Header().Set("Content-Type", "application/connect+proto")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	// Headers have to leave before the next frame. A reconnect often
	// finds an empty queue, and an unflushed WriteHeader never reaches
	// the client until something is pushed.
	if fl != nil {
		fl.Flush()
	}
	for {
		frame, ok := s.next(r.Context(), watch)
		if !ok {
			return
		}
		if _, werr := w.Write(frame); werr != nil {
			return
		}
		if fl != nil {
			fl.Flush()
		}
	}
}

func (p *Proxy) bidi(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	body, err := readLimited(r.Body, maxAppendBody)
	if err != nil {
		bidiLog("read-fail reason=too-large err=%v enc=%q", err, r.Header.Get("Content-Encoding"))
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	body, gunzipped := gunzipRequestLimit(body, r.Header.Get("Content-Encoding"), maxAppendBody)
	if gunzipped {
		bidiLog("gunzipped len=%d", len(body))
	}
	plain, err := inspectUnary(body)
	if err != nil {
		bidiLog("unwrap-fail err=%v len=%d forward", err, len(body))
		p.forward(w, r, body, gunzipped)
		return
	}
	id, data, blob, err := decodeAppend(plain)
	if err != nil {
		bidiLog("decodeAppend-fail err=%v plain=%d", err, len(plain))
		http.Error(w, "append", http.StatusBadRequest)
		return
	}
	if id == "" {
		bidiLog("forward no-id dataLen=%d blobLen=%d", len(data), len(blob))
		p.forward(w, r, body, gunzipped)
		return
	}
	var raw []byte
	switch {
	case len(blob) > 0:
		// Cursor 3.x may send data_binary instead of hex data.
		raw = blob
		bidiLog("append id=%s via=data_binary n=%d", id, len(raw))
	case len(data) > 0:
		raw = make([]byte, hex.DecodedLen(len(data)))
		n, err := hex.Decode(raw, data)
		if err != nil {
			bidiLog("hex-fail err=%v dataLen=%d", err, len(data))
			http.Error(w, "append", http.StatusBadRequest)
			return
		}
		raw = raw[:n]
		bidiLog("append id=%s via=hex n=%d", id, len(raw))
	default:
		bidiLog("forward empty-payload id=%s", id)
		p.forward(w, r, body, gunzipped)
		return
	}
	if s := p.hub.localOf(id); s != nil {
		p.localAppend(w, s, raw, time.Since(started).Milliseconds())
		return
	}
	if run, ok := inferenceRun(raw); ok {
		route, ours, err := RouteModel(p.Models(), run.ModelID, run.Params)
		if err != nil {
			bidiLog("route-fail model=%q err=%v", run.ModelID, err)
			http.Error(w, "model", http.StatusBadRequest)
			return
		}
		if !ours {
			Note(fmt.Sprintf("route miss model=%s path=BidiAppend forward", run.ModelID))
			p.hub.mark(id, false, Route{}, "")
			p.forward(w, r, body, gunzipped)
			return
		}
		Note(fmt.Sprintf("route ok model=%s path=BidiAppend local", route.Model))
		s := p.hub.mark(id, true, route, run.ConversationID)
		s.push(runReady(route.Model, route.Effort, route.ContextTokens))
		writeProto(w, nil, "application/proto")
		return
	}
	p.forward(w, r, body, gunzipped)
}

func (p *Proxy) localAppend(w http.ResponseWriter, s *sess, raw []byte, appendMs int64) {
	// Cursor 3.x sometimes nests a Connect frame inside BidiAppend data.
	if plain, framed, uerr := UnwrapUnary(raw); uerr == nil && framed && len(plain) > 0 {
		bidiLog("localAppend unwrap nested connect n=%d->%d", len(raw), len(plain))
		raw = plain
	}
	compileStart := time.Now()
	c, err := ReadClient(raw)
	compileMs := time.Since(compileStart).Milliseconds()
	if err != nil {
		bidiLog("ReadClient-fail n=%d err=%v", len(raw), err)
		http.Error(w, "append", http.StatusBadRequest)
		return
	}
	switch c.Kind {
	case KindInvoke:
		chat, err := chatFromRead(s.route, s.conv, c)
		if err != nil {
			id := c.InvocationID
			if id == "" {
				id = "invoke"
			}
			bidiLog("CompileChat-fail inv=%q err=%v", id, err)
			s.push(invocationIdFrame(id))
			s.push(endFrame(id, err.Error()))
		} else {
			ctx, gen := s.beginRun(chat.InvocationID)
			go p.runChat(s, ctx, gen, chat, appendMs, compileMs)
		}
	case KindCancel:
		// Stops this invocation. The SSE session and its parent context
		// stay, so the next invoke on this request id can run.
		s.stopRun()
	case KindFinish:
		s.finish()
	}
	writeProto(w, nil, "application/proto")
}

func (p *Proxy) runChat(s *sess, ctx context.Context, gen uint64, chat Chat, appendMs, compileMs int64) {
	noteBidiHistory(chat)
	defer Note("chat done model=" + chat.Model + " path=BidiAppend")
	if ctx.Err() != nil {
		return
	}
	if !s.pushRun(gen, invocationIdFrame(chat.InvocationID)) {
		return
	}
	var sawText bool
	// bare counts rounds whose calls were all refused by the facade.
	// Cursor never sees those calls, so it will not send a follow-up
	// invoke. One such round is asked again with the tool error and the
	// images still on the chat. A second is the model repeating that
	// shape; the turn stops instead of spinning. A call that can run is
	// one completing tool_call_part, and Cursor's own loop continues.
	bare := 0
	for {
		if ctx.Err() != nil {
			return
		}
		run, skipped, err := p.completeInference(ctx, s, gen, chat, &sawText, appendMs, compileMs)
		appendMs, compileMs = 0, 0
		if errors.Is(err, errStopped) || ctx.Err() != nil {
			return
		}
		if err != nil {
			s.endRun(gen, chat.InvocationID, err.Error(), false)
			return
		}
		Note(fmt.Sprintf("agent tool round runnable=%d skipped=%d images=%d", len(run), len(skipped), countChatImages(chat.Messages)))
		if len(run) > 0 || len(skipped) == 0 {
			break
		}
		bare++
		if bare > 1 {
			Note("agent tool round stop reason=bare")
			break
		}
		feedSkipped(&chat, skipped)
	}
	s.endRun(gen, chat.InvocationID, "", sawText)
}

// completeInference asks the gateway once and writes text as it arrives.
// Runnable calls go out as one completing tool_call_part each. Skipped
// calls are not sent. The error is the frame text.
func (p *Proxy) completeInference(ctx context.Context, s *sess, gen uint64, chat Chat, sawText *bool, appendMs, compileMs int64) (run, skipped []toolBuf, err error) {
	noteRequestToolArgs(chat.Messages)
	jsonStart := time.Now()
	body, jerr := chat.JSON()
	jsonMs := time.Since(jsonStart).Milliseconds()
	if jerr != nil {
		return nil, nil, errf("chat")
	}
	reqStart := time.Now()
	resp, derr := p.postChat(ctx, chat, body)
	ttfb := time.Since(reqStart).Milliseconds()
	// A bare retry zeros append-ms and compile-ms. The cache counts on
	// chat are from the invoke's compile, already logged on the first
	// line. Repeating them makes the retry look like another compile.
	hit, miss := chat.cacheHit, chat.cacheMiss
	if appendMs == 0 && compileMs == 0 {
		hit, miss = -1, -1
	}
	noteBidiTiming(appendMs, compileMs, jsonMs, ttfb, len(body), hit, miss)
	if ctx.Err() != nil || errors.Is(derr, context.Canceled) {
		return nil, nil, errStopped
	}
	if derr != nil {
		return nil, nil, errf("gateway")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("cursor ide: gateway status %d", resp.StatusCode)
	}
	var asm toolAssembler
	var said strings.Builder
	var firstText int64 = -1
	readErr := eachPiece(resp.Body, func(piece chatPiece) error {
		if ctx.Err() != nil {
			return errStopped
		}
		if piece.thinking != "" {
			if !s.pushRun(gen, thinkingFrame(chat.InvocationID, piece.thinking)) {
				return errStopped
			}
			if firstText < 0 {
				firstText = time.Since(reqStart).Milliseconds()
			}
			said.WriteString(piece.thinking)
			said.WriteByte('\n')
		}
		// Cursor drops text on is_final and only emits finish; content
		// must ride non-final text_part frames (#Cursor 3.27 local loop).
		if piece.text != "" {
			*sawText = true
			if !s.pushRun(gen, textFrame(chat.InvocationID, piece.text, false)) {
				return errStopped
			}
			if firstText < 0 {
				firstText = time.Since(reqStart).Milliseconds()
			}
			said.WriteString(piece.text)
			said.WriteByte('\n')
		}
		// Fragments stay in the assembler. A finish_reason is not the
		// end: a duplicate completion often follows it in the same body,
		// and a fragment already written cannot be taken back. Cursor
		// concatenates tool_call_part until is_complete, so an early
		// "{}" or a prefix plus the later object is the Read {} it runs.
		for _, d := range piece.tools {
			asm.add(d)
		}
		return nil
	})
	if ctx.Err() != nil || errors.Is(readErr, errStopped) || errors.Is(readErr, context.Canceled) {
		return nil, nil, errStopped
	}
	if readErr != nil {
		Note("chat fail path=BidiAppend reason=stream")
		return nil, nil, readErr
	}
	// A clean end still seals, including a body with no [DONE]. A stream
	// error or a canceled read returned above, so this seal is only a
	// body that finished.
	run, skipped, frames := finishInferenceTools(chat.InvocationID, &asm, said.String())
	sent := true
	if len(frames) > 0 {
		sent = s.emitRun(gen, frames)
	}
	markers := markupMarkers(said.String())
	state := "skipped"
	if sent {
		state = "written"
	}
	for _, t := range run {
		noteToolTrace("frames", t.name, t.args, t.frags, t.placeholders, t.fill, markers, state)
	}
	Note(fmt.Sprintf("bidi timing first-text-ms=%d seal-ms=%d", firstText, time.Since(reqStart).Milliseconds()))
	if ctx.Err() != nil || !sent {
		return nil, nil, errStopped
	}
	return run, skipped, nil
}

// postChat is the gateway chat completion both inject paths send.
// Accept-Encoding is identity so a stream is not gunzipped by the
// transport: a gzip member buffers until it can flush, which holds the
// first token. The body is the JSON the caller already built.
func (p *Proxy) postChat(ctx context.Context, chat Chat, body []byte) (*http.Response, error) {
	base, token := p.Gateway()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range chat.Header() {
		req.Header.Set(k, v)
	}
	return p.client.Do(req)
}

// noteBidiTiming is the prep line for one BidiAppend gateway call.
// append-ms is read, gunzip and hex. compile-ms is the one protobuf
// compile. json-ms is the gateway body. gateway-ttfb is the request
// start until response headers. A bare retry did not read a new append,
// so those two are 0, and hit and miss are negative: the cache counts
// stay off that line. Counts and durations only: no text, paths, or
// image bytes.
func noteBidiTiming(appendMs, compileMs, jsonMs, ttfb int64, jsonBytes, hit, miss int) {
	if hit < 0 || miss < 0 {
		Note(fmt.Sprintf("bidi timing append-ms=%d compile-ms=%d json-ms=%d gateway-ttfb=%d json-bytes=%d",
			appendMs, compileMs, jsonMs, ttfb, jsonBytes))
		return
	}
	Note(fmt.Sprintf("bidi timing append-ms=%d compile-ms=%d json-ms=%d gateway-ttfb=%d json-bytes=%d cache-hit=%d cache-miss=%d",
		appendMs, compileMs, jsonMs, ttfb, jsonBytes, hit, miss))
}

// noteBidiHistory is the per-invoke line for the path injected Agent
// chats actually take. The transcript is inside the invoke. There is
// no KvServerMessage on RunInference, so history is inline. Counts are
// what the gateway is about to be asked. Text, paths, and arguments
// are not on the line.
func noteBidiHistory(chat Chat) {
	var calls, results, empty, reasoning, n int
	for _, m := range chat.Messages {
		if m.Reasoning != "" {
			reasoning++
			n += len(m.Reasoning)
		}
		switch c := m.Content.(type) {
		case string:
			n += len(c)
		case []ContentPart:
			for _, p := range c {
				if p.Type == "text" {
					n += len(p.Text)
				}
			}
		}
		if m.Role == "tool" {
			results++
		}
		for _, c := range m.ToolCalls {
			calls++
			n += len(c.Arguments)
			if callArgsEmpty(c.Arguments) {
				empty++
			}
		}
	}
	var imageBytes int
	for _, m := range chat.Messages {
		for _, img := range messageImages(m) {
			imageBytes += imageByteCount(img)
		}
	}
	Note(fmt.Sprintf("chat start model=%s path=BidiAppend history=inline messages=%d tools=%d calls=%d results=%d images=%d bytes=%d empty-args=%d reasoning=%d image-bytes=%d",
		chat.Model, len(chat.Messages), len(chat.Tools), calls, results, countChatImages(chat.Messages), n, empty, reasoning, imageBytes))
}

// agentHistoryMode is how this AgentService/Run got its prompt.
// kv means the body is still open and blob ids will be asked for.
// blobs means holes remain on a run that cannot ask. inline means
// conversation_state already resolved, the same word BidiAppend uses
// when the invoke carried the transcript itself.
func agentHistoryMode(turn AgentTurn) string {
	if turn.liveKV {
		return "kv"
	}
	if turn.MissingBlobs > 0 {
		return "blobs"
	}
	return "inline"
}

// finishInferenceTools writes one completing tool_call_part per call that
// Pi can run. The part's arguments are the sealed JSON, not the unsent
// tail of a fragment. Cursor's local loop parses that part and runs the
// tool. A call that is still missing its strings is not sent.
func finishInferenceTools(invocation string, asm *toolAssembler, text string) (run, skipped []toolBuf, frames [][]byte) {
	run, skipped = sealCalls(asm, text, func(msg string) {
		frames = append(frames, textFrame(invocation, msg, false))
	})
	for _, t := range run {
		frames = append(frames, toolCallFrame(invocation, t.id, t.name, displayArgs(t.name, t.args), true))
	}
	return run, skipped, frames
}

// agentRun answers AgentService/Run when the model is in the catalog.
// Cursor's backend is what returns ERROR_BAD_MODEL_NAME / Unknown model
// ID for a provider/model id, so that run is not forwarded. A model
// Cursor itself lists is forwarded unchanged.
//
// When the client keeps the request body open (chunked) after the first
// Connect frame, later frames carry tool results for the same turn.
// Reading the whole body before responding prevented that loop on HTTP/1.1.
func (p *Proxy) agentRun(w http.ResponseWriter, r *http.Request) {
	streaming := r.ContentLength < 0 || r.Header.Get("Transfer-Encoding") != ""
	if streaming {
		p.agentRunStreaming(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(maxCatalogBody)+1))
	if err != nil || len(body) > maxCatalogBody {
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	body, gunzipped := gunzipRequest(body, r.Header.Get("Content-Encoding"))
	turn, ok, err := ReadAgentRun(body)
	if err != nil && catalogRefusal(p, turn) {
		Note("agent-run history fail")
		http.Error(w, "history", http.StatusBadRequest)
		return
	}
	if err != nil || !ok {
		Note("agent-run forward reason=not-a-run")
		p.forward(w, r, body, gunzipped)
		return
	}
	var followups chan []byte
	if payloads, framed := connectPayloads(body); framed && len(payloads) > 1 {
		followups = make(chan []byte, len(payloads)-1)
		for _, extra := range payloads[1:] {
			followups <- extra
		}
		close(followups)
	}
	p.finishAgentRun(w, r, turn, body, gunzipped, followups, nil)
}

func (p *Proxy) agentRunStreaming(w http.ResponseWriter, r *http.Request) {
	// The IDE keeps this body open and reads the response at the same
	// time. Without this, net/http finishes the body before the first
	// response byte, and an official run never starts.
	enableFullDuplex(w)
	bodyReader := io.Reader(r.Body)
	gunzipped := false
	if enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); enc == "gzip" || enc == "x-gzip" {
		body, err := io.ReadAll(io.LimitReader(r.Body, int64(maxCatalogBody)+1))
		if err != nil || len(body) > maxCatalogBody {
			http.Error(w, "read", http.StatusBadRequest)
			return
		}
		var plain bool
		body, plain = gunzipRequest(body, r.Header.Get("Content-Encoding"))
		gunzipped = plain
		bodyReader = bytes.NewReader(body)
	}
	caught := newPrefixCapture(bodyReader)
	fr := newConnectFrameReader(caught)
	first, err := fr.next()
	head := caught.prefix()
	caught.stop()
	// head is the wire bytes already read. bodyReader is positioned
	// just after them. Forwarding waits for neither the client to
	// close the body nor the upstream to finish before the other
	// starts: an official run keeps the body open for later tool
	// results, and Cursor stalls if those frames sit in this proxy.
	forwardStreaming := func() {
		p.forwardLive(w, r, head, bodyReader, gunzipped)
	}
	if err != nil {
		Note("agent-run forward reason=not-a-run")
		forwardStreaming()
		return
	}
	turn, ok, err := readAgentClient(first)
	if err != nil && catalogRefusal(p, turn) {
		Note("agent-run history fail")
		http.Error(w, "history", http.StatusBadRequest)
		return
	}
	if err != nil || !ok {
		Note("agent-run forward reason=not-a-run")
		forwardStreaming()
		return
	}
	route, ours, rerr := RouteModel(p.Models(), turn.ModelID, turn.Params)
	if rerr != nil {
		Note(fmt.Sprintf("route fail model=%s path=AgentService/Run", turn.ModelID))
		http.Error(w, "model", http.StatusBadRequest)
		return
	}
	if !ours {
		Note(fmt.Sprintf("route miss model=%s path=AgentService/Run forward", turn.ModelID))
		forwardStreaming()
		return
	}
	pumpCtx, stopPump := context.WithCancel(r.Context())
	followups := make(chan []byte, 8)
	go pumpConnectFrames(pumpCtx, fr, followups)
	p.serveLocalAgentRun(w, r, turn, route, followups, stopPump)
}

func pumpConnectFrames(ctx context.Context, fr *connectFrameReader, out chan<- []byte) {
	defer close(out)
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		payload, err := fr.next()
		if err != nil {
			return
		}
		select {
		case out <- payload:
		case <-ctx.Done():
			return
		}
	}
}

func (p *Proxy) finishAgentRun(w http.ResponseWriter, r *http.Request, turn AgentTurn, forwardBody []byte, gunzipped bool, followups <-chan []byte, stopPump context.CancelFunc) {
	route, ours, err := RouteModel(p.Models(), turn.ModelID, turn.Params)
	if err != nil {
		Note(fmt.Sprintf("route fail model=%s path=AgentService/Run", turn.ModelID))
		http.Error(w, "model", http.StatusBadRequest)
		return
	}
	if !ours {
		Note(fmt.Sprintf("route miss model=%s path=AgentService/Run forward", turn.ModelID))
		if stopPump != nil {
			stopPump()
			drainFollowups(followups)
		}
		p.forward(w, r, forwardBody, gunzipped)
		return
	}
	p.serveLocalAgentRun(w, r, turn, route, followups, stopPump)
}

func (p *Proxy) serveLocalAgentRun(w http.ResponseWriter, r *http.Request, turn AgentTurn, route Route, followups <-chan []byte, stopPump context.CancelFunc) {
	Note(fmt.Sprintf("route ok model=%s path=AgentService/Run local messages=%d tools=%d blobs-missing=%d images=%d", route.Model, len(turn.Messages), len(turn.Tools), turn.MissingBlobs, countChatImages(turn.Messages)))
	w.Header().Set("Content-Type", "application/connect+proto")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	write := func(frame []byte) {
		_, _ = w.Write(frame)
		if fl != nil {
			fl.Flush()
		}
	}
	// A live body can still fill an empty prompt: the history is blob
	// ids, and the client has not been asked yet. A buffered body cannot.
	if stopPump != nil && len(turn.rawRun) > 0 && len(missingBlobIDs(turn.rawRun, turn.blobs, nil)) > 0 {
		turn.liveKV = true
	}
	if len(turn.Messages) == 0 && !turn.liveKV {
		Note("chat done model=" + route.Model + " path=AgentService/Run reason=no-message")
		write(agentTextFrame("cursor ide: agent run has no message"))
		write(agentTurnEnded())
		write(connectEnd())
		if stopPump != nil {
			stopPump()
			drainFollowups(followups)
		}
		return
	}
	if stopPump != nil {
		defer func() {
			stopPump()
			drainFollowups(followups)
		}()
	}
	Note(fmt.Sprintf("chat start model=%s path=AgentService/Run history=%s", route.Model, agentHistoryMode(turn)))
	defer Note("chat done model=" + route.Model + " path=AgentService/Run")
	p.runAgentLoop(r.Context(), followups, turn, route, write)
}

func writeSealedTools(asm *toolAssembler, execN *uint32, write func([]byte)) {
	for _, t := range asm.seal() {
		*execN++
		frames, ok := agentToolFrames(t.id, t.name, t.args, *execN)
		if !ok {
			write(agentTextFrame("cursor ide: tool arguments are not json"))
			continue
		}
		for _, frame := range frames {
			write(frame)
		}
	}
}

func eachPiece(r io.Reader, fn func(chatPiece) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		piece, ok := chatPieces(sc.Text())
		if !ok {
			continue
		}
		if piece.fail != "" {
			return &streamFail{msg: piece.fail}
		}
		if err := fn(piece); err != nil {
			return err
		}
		if piece.done {
			return nil
		}
	}
	err := sc.Err()
	if errors.Is(err, bufio.ErrTooLong) {
		return errf("gateway stream line too large")
	}
	return err
}

func (p *Proxy) fetch(r *http.Request, body []byte) (int, []byte, string, error) {
	// The IDE often sends Accept-Encoding: br. Forwarding that makes
	// Cursor's backend answer with brotli, which is not protobuf, and
	// appending to it drops official rows (Grok included) out of the
	// list the IDE can parse. identity keeps a successful reply as
	// the protobuf MergeUnary appends to.
	hdr := r.Header.Clone()
	if hdr == nil {
		hdr = make(http.Header)
	}
	hdr.Set("Accept-Encoding", "identity")
	resp, err := p.doReader(r, bytes.NewReader(body), false, int64(len(body)), hdr)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxCatalogBody)+1))
	if err != nil {
		return 0, nil, "", err
	}
	if len(b) > maxCatalogBody {
		return 0, nil, "", errf("upstream catalog too large")
	}
	b, err = decodeCatalogBody(b, resp.Header.Get("Content-Encoding"))
	if err != nil {
		return 0, nil, "", err
	}
	return resp.StatusCode, b, resp.Header.Get("Content-Type"), nil
}

// decodeCatalogBody gunzips a catalog reply when Cursor's Accept-Encoding
// left the upstream body compressed. MergeUnary needs raw protobuf.
func decodeCatalogBody(b []byte, contentEncoding string) ([]byte, error) {
	plain, err := gunzipUpTo(b, contentEncoding, maxCatalogBody)
	if err != nil && err.Error() == "cursor ide: body too large" {
		return nil, errf("upstream catalog too large")
	}
	return plain, err
}

func gunzipUpTo(b []byte, contentEncoding string, limit int) ([]byte, error) {
	enc := strings.ToLower(contentEncoding)
	if enc != "gzip" && !(len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b) {
		return b, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	plain, err := io.ReadAll(io.LimitReader(zr, int64(limit)+1))
	_ = zr.Close()
	if err != nil {
		return nil, err
	}
	if len(plain) > limit {
		return nil, errf("body too large")
	}
	return plain, nil
}

// gunzipRequest is the body to inspect. plain is true only when those
// bytes are no longer gzip: a later forward must drop Content-Encoding
// and send Content-Length for this slice. A failed gunzip keeps the
// original bytes and plain false, so the headers the client sent still
// match. x-gzip is the same encoding.
func gunzipRequest(body []byte, contentEncoding string) ([]byte, bool) {
	return gunzipRequestLimit(body, contentEncoding, maxCatalogBody)
}

// gunzipRequestLimit is gunzipRequest with an explicit cap. BidiAppend
// uses maxAppendBody so a gzipped burst of images is not cut at the
// catalog cap. A failed gunzip keeps the original bytes.
func gunzipRequestLimit(body []byte, contentEncoding string, limit int) ([]byte, bool) {
	enc := strings.ToLower(strings.TrimSpace(contentEncoding))
	magic := len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b
	if enc != "gzip" && enc != "x-gzip" && !magic {
		return body, false
	}
	plain, err := gunzipUpTo(body, contentEncoding, limit)
	if err != nil || bytes.Equal(plain, body) {
		if err != nil {
			bidiLog("gunzip-fail err=%v len=%d", err, len(body))
		}
		return body, false
	}
	return plain, true
}

func readLimited(r io.Reader, limit int) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > limit {
		return nil, errf("body too large")
	}
	return b, nil
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, body []byte, plain bool) {
	if body == nil {
		b, err := readLimited(r.Body, maxCatalogBody)
		if err != nil {
			http.Error(w, "read", http.StatusBadRequest)
			return
		}
		body = b
		plain = false
	}
	resp, err := p.do(r, body, plain)
	if err != nil {
		http.Error(w, "upstream", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	writeUpstream(w, resp)
}

// forwardLive sends head and then rest without waiting for rest to end
// before the response is written back. rest may stay open for the whole
// turn. A live body is chunked so the backend can answer before the
// client closes it.
func (p *Proxy) forwardLive(w http.ResponseWriter, r *http.Request, head []byte, rest io.Reader, plain bool) {
	if rest == nil {
		p.forward(w, r, head, plain)
		return
	}
	// An in-memory body (the HTTP-gzip path) has a known length. Send
	// that length. A live body stays chunked so the backend can answer
	// before the client closes it.
	if br, ok := rest.(*bytes.Reader); ok {
		tail, err := io.ReadAll(br)
		if err != nil {
			http.Error(w, "read", http.StatusBadRequest)
			return
		}
		p.forward(w, r, append(append([]byte(nil), head...), tail...), plain)
		return
	}
	pr, pw := io.Pipe()
	go func() {
		var err error
		if len(head) > 0 {
			_, err = pw.Write(head)
		}
		if err == nil {
			_, err = io.Copy(pw, rest)
		}
		_ = pw.CloseWithError(err)
	}()
	resp, err := p.doReader(r, pr, plain, -1, nil)
	if err != nil {
		_ = pr.CloseWithError(err)
		http.Error(w, "upstream", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	writeUpstream(w, resp)
}

func (p *Proxy) do(r *http.Request, body []byte, plain bool) (*http.Response, error) {
	return p.doReader(r, bytes.NewReader(body), plain, int64(len(body)), nil)
}

func (p *Proxy) doReader(r *http.Request, body io.Reader, plain bool, n int64, hdr http.Header) (*http.Response, error) {
	if p.client == nil {
		return nil, errf("no upstream client")
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, p.origin(r), body)
	if err != nil {
		return nil, err
	}
	// body is what is sent. The client's Content-Length is the size before
	// a gunzip, so it is not copied: the Transport writes this field.
	// n < 0 is a body that is still open; chunked framing lets the
	// backend answer before the client closes it.
	if n >= 0 {
		req.ContentLength = n
	} else {
		req.ContentLength = -1
	}
	src := r.Header
	if hdr != nil {
		src = hdr
	}
	for k, vs := range src {
		if hop(k) || strings.EqualFold(k, "Host") || strings.EqualFold(k, "Content-Length") {
			continue
		}
		// plain means the body was gunzipped for inspection. Leaving
		// Content-Encoding: gzip on those bytes is what Cursor logs as
		// bidiAppend "decompression failed"; the stream then never ends.
		if plain && strings.EqualFold(k, "Content-Encoding") {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if n >= 0 {
		req.Header.Set("Content-Length", strconv.FormatInt(n, 10))
	} else {
		req.Header.Del("Content-Length")
	}
	if plain {
		req.Header.Del("Content-Encoding")
	}
	return p.client.Do(req)
}

// writeUpstream copies an upstream response as it arrives. The IDE's
// HTTP server buffer holds a short frame until the handler returns, which
// is long enough for Cursor to call an official stream stalled. Grok's
// first tokens and the HTTP/1.1 heartbeats are those short frames.
func writeUpstream(w http.ResponseWriter, resp *http.Response) {
	for k, vs := range resp.Header {
		if hop(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			break
		}
	}
	for k, vs := range resp.Trailer {
		if hop(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
}

// enableFullDuplex lets the handler write the response while the
// request body is still open. A writer that cannot (a test stand-in)
// keeps the ordinary half-duplex behavior.
func enableFullDuplex(w http.ResponseWriter) {
	_ = http.NewResponseController(w).EnableFullDuplex()
}

// catalogRefusal is true when a parsed run names a magpie model. A
// truncated conversation_state on that id is refused. The same body for
// any other id is forwarded.
func catalogRefusal(p *Proxy, turn AgentTurn) bool {
	if turn.ModelID == "" {
		return false
	}
	_, ours, err := RouteModel(p.Models(), turn.ModelID, turn.Params)
	return ours || err != nil
}

// upstreamTransport talks to Cursor the way the IDE does while this proxy
// is connected. disableHttp2 has put the IDE on HTTP/1.1. Upgrading that
// hop to HTTP/2, or adding gzip the IDE did not ask for, changes the
// framing official models are written against.
func upstreamTransport() *http.Transport {
	return &http.Transport{
		// A nil Proxy would also skip HTTP_PROXY. Set it so a later edit
		// cannot inherit ProxyFromEnvironment and send Cursor's traffic
		// back into this proxy.
		Proxy:               func(*http.Request) (*url.URL, error) { return nil, nil },
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
		DisableCompression:  true,
		ForceAttemptHTTP2:   false,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
}

// prefixCapture records the bytes a frame reader consumes, then stops.
// The forwarded official run is those bytes plus whatever is still in
// the body, not a re-encoded frame.
type prefixCapture struct {
	r   io.Reader
	buf bytes.Buffer
	on  bool
}

func newPrefixCapture(r io.Reader) *prefixCapture {
	return &prefixCapture{r: r, on: true}
}

func (c *prefixCapture) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if c.on && n > 0 {
		c.buf.Write(p[:n])
	}
	return n, err
}

func (c *prefixCapture) prefix() []byte {
	return append([]byte(nil), c.buf.Bytes()...)
}

func (c *prefixCapture) stop() { c.on = false }

func (p *Proxy) origin(r *http.Request) string {
	path := r.URL.RequestURI()
	if path == "" {
		path = r.URL.Path
	}
	if p.Upstream != "" {
		return strings.TrimRight(p.Upstream, "/") + path
	}
	host := r.Host
	if host == "" {
		host = "api2.cursor.sh"
	}
	return "https://" + host + path
}

func hop(k string) bool {
	switch strings.ToLower(k) {
	case "connection", "proxy-connection", "keep-alive", "transfer-encoding", "te", "trailer", "upgrade", "proxy-authenticate", "proxy-authorization":
		return true
	default:
		return false
	}
}

func isCursorHost(host string) bool {
	host = hostOnly(host)
	return host == "api2.cursor.sh" || host == "api3.cursor.sh" || strings.HasSuffix(host, ".cursor.sh")
}

func hostOnly(host string) string {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	return host
}

func tunnel(client net.Conn, host string) {
	defer client.Close()
	up, err := net.DialTimeout("tcp", host, 10*time.Second)
	if err != nil {
		return
	}
	defer up.Close()
	go io.Copy(up, client)
	io.Copy(client, up)
}

// singleListener gives http.Server one connection. Accept blocks after
// that until the connection closes, and Close does not close it early:
// Server.Serve closes its listener when Accept returns, which would cut
// off a Cursor session that still has requests to send.
type singleListener struct {
	conn net.Conn
	mu   sync.Mutex
	gave bool
	stop chan struct{}
	once sync.Once
}

func newSingleListener(c net.Conn) *singleListener {
	return &singleListener{conn: c, stop: make(chan struct{})}
}

func (l *singleListener) signal() { l.once.Do(func() { close(l.stop) }) }

func (l *singleListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.gave {
		l.gave = true
		c := l.conn
		l.mu.Unlock()
		return &watchConn{Conn: c, done: l.signal}, nil
	}
	l.mu.Unlock()
	<-l.stop
	return nil, net.ErrClosed
}

func (l *singleListener) Close() error {
	l.signal()
	return l.conn.Close()
}

func (l *singleListener) Addr() net.Addr { return l.conn.LocalAddr() }

type watchConn struct {
	net.Conn
	once sync.Once
	done func()
}

func (c *watchConn) Close() error {
	c.once.Do(c.done)
	return c.Conn.Close()
}

func sseRequestID(body []byte) (string, error) {
	plain, _, err := UnwrapUnary(body)
	if err != nil {
		return "", err
	}
	fs, err := parseFields(plain)
	if err != nil {
		return "", err
	}
	id, _ := fieldString(fs, 1)
	return id, nil
}

func decodeAppend(plain []byte) (id string, data, blob []byte, err error) {
	fs, err := parseFields(plain)
	if err != nil {
		return "", nil, nil, err
	}
	// data is the hex of the invoke. It stays a slice of plain so the
	// decode does not copy the hex before turning it into protobuf.
	for _, f := range fs {
		if f.num == 1 && f.wt == 2 {
			data = f.b
			break
		}
	}
	if bins := fieldBytes(fs, 4); len(bins) > 0 && len(bins[0]) > 0 {
		blob = bins[0]
	}
	if ids := fieldBytes(fs, 2); len(ids) > 0 {
		inner, err := parseFields(ids[0])
		if err != nil {
			return "", nil, nil, err
		}
		id, _ = fieldString(inner, 1)
	}
	return id, data, blob, nil
}

// inferenceRun is a RunInference run_request. Its conversation id is a
// UUID. The agent protocol's run request is a message in that same field,
// which is not a UUID, so it is forwarded rather than handled here.
func inferenceRun(payload []byte) (Client, bool) {
	c, err := ReadClient(payload)
	if err != nil || c.Kind != KindRun || c.ModelID == "" || !isUUID(c.ConversationID) {
		return Client{}, false
	}
	return c, true
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !unicode.Is(unicode.Hex_Digit, c) {
				return false
			}
		}
	}
	return true
}

// frameQueueMax is how many unread inference frames one session holds.
// A later RunInferenceSSE drains them. Past the cap the producer waits
// instead of keeping the rest of the completion in memory. The wait ends
// when a reader takes a frame, this invocation is canceled, or the proxy
// closes. The reader leaving does not cancel the invocation: Cursor opens
// the stream again for the same request id.
const frameQueueMax = 256

// errStopped is an invocation ended by KindCancel, replaced by a newer
// invoke, or cut by the proxy closing. It is not a provider failure.
var errStopped = errors.New("cursor ide: stopped")

// streamFail is a provider error carried in a 200 chat stream.
type streamFail struct{ msg string }

func (e *streamFail) Error() string {
	if e == nil || e.msg == "" {
		return "cursor ide: gateway stream error"
	}
	return e.msg
}

type hub struct {
	mu     sync.Mutex
	m      map[string]*sess
	closed bool
}

func newHub() *hub { return &hub{m: map[string]*sess{}} }

func (h *hub) sess(id string) *sess {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.m[id]; s != nil {
		return s
	}
	if h.closed {
		return closedSess(h, id)
	}
	s := newSess(h, id)
	h.m[id] = s
	return s
}

func (h *hub) mark(id string, local bool, route Route, conv string) *sess {
	s := h.sess(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.decided || s.done {
		return s
	}
	s.local, s.route, s.conv = local, route, conv
	s.decided = true
	close(s.ready)
	return s
}

func (h *hub) localOf(id string) *sess {
	h.mu.Lock()
	s := h.m[id]
	h.mu.Unlock()
	if s == nil || !s.isLocal() {
		return nil
	}
	return s
}

func (h *hub) stopAll() {
	h.mu.Lock()
	h.closed = true
	all := h.m
	h.m = map[string]*sess{}
	h.mu.Unlock()
	for _, s := range all {
		s.shutdown()
	}
}

func (h *hub) dropIfIdle(s *sess) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done && s.readers == 0 && h.m[s.id] == s {
		delete(h.m, s.id)
	}
}

type sess struct {
	id      string
	hub     *hub
	mu      sync.Mutex
	local   bool
	decided bool
	ready   chan struct{}
	route   Route
	conv    string
	// parent outlives every invocation. KindCancel does not cancel it.
	parent       context.Context
	parentCancel context.CancelFunc
	runCtx       context.Context
	runCancel    context.CancelFunc
	runGen       uint64
	runInv       string
	runEnded     bool
	frames       [][]byte
	note         chan struct{}
	space        chan struct{}
	done         bool
	finished     bool
	readers      int
	readerGen    uint64
	watchCancel  context.CancelFunc
}

func newSess(h *hub, id string) *sess {
	s := &sess{
		id:    id,
		hub:   h,
		ready: make(chan struct{}),
		note:  make(chan struct{}),
		space: make(chan struct{}),
	}
	s.parent, s.parentCancel = context.WithCancel(context.Background())
	return s
}

func closedSess(h *hub, id string) *sess {
	s := newSess(h, id)
	s.done = true
	s.finished = true
	s.decided = true
	close(s.ready)
	s.parentCancel()
	return s
}

func (s *sess) wait(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-s.ready:
		return true
	}
}

func (s *sess) isLocal() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.decided && s.local
}

// attach is one RunInferenceSSE reader. A newer reader cancels the watch
// so the previous handler stops reading. The gateway call is not canceled.
func (s *sess) attach() (context.Context, func()) {
	s.mu.Lock()
	if s.watchCancel != nil {
		s.watchCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.readerGen++
	gen := s.readerGen
	s.watchCancel = cancel
	s.readers++
	s.mu.Unlock()
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			s.mu.Lock()
			s.readers--
			if s.readerGen == gen {
				s.watchCancel = nil
			}
			s.mu.Unlock()
			if s.hub != nil {
				s.hub.dropIfIdle(s)
			}
		})
	}
}

func (s *sess) beginRun(inv string) (context.Context, uint64) {
	s.mu.Lock()
	if s.done {
		ctx := s.parent
		gen := s.runGen
		s.mu.Unlock()
		return ctx, gen
	}
	if !s.runEnded && s.runInv != "" {
		s.queueLocked(endFrame(s.runInv, ""))
	}
	prev := s.runCancel
	s.runGen++
	gen := s.runGen
	s.runInv = inv
	s.runEnded = false
	ctx, cancel := context.WithCancel(s.parent)
	s.runCtx = ctx
	s.runCancel = cancel
	s.mu.Unlock()
	if prev != nil {
		prev()
	}
	return ctx, gen
}

func (s *sess) stopRun() {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return
	}
	s.runGen++
	prev := s.runCancel
	s.runCancel = nil
	if !s.runEnded && s.runInv != "" {
		s.runEnded = true
		s.queueLocked(endFrame(s.runInv, ""))
	}
	s.mu.Unlock()
	if prev != nil {
		prev()
	}
}

func (s *sess) push(b []byte) {
	for {
		s.mu.Lock()
		if s.done {
			s.mu.Unlock()
			return
		}
		if len(s.frames) < frameQueueMax {
			s.queueLocked(b)
			s.mu.Unlock()
			return
		}
		space := s.space
		parent := s.parent
		s.mu.Unlock()
		select {
		case <-space:
		case <-parent.Done():
			return
		}
	}
}

func (s *sess) pushRun(gen uint64, b []byte) bool {
	for {
		s.mu.Lock()
		if s.done || gen != s.runGen {
			s.mu.Unlock()
			return false
		}
		if len(s.frames) < frameQueueMax {
			s.queueLocked(b)
			s.mu.Unlock()
			return true
		}
		space := s.space
		runCtx := s.runCtx
		parent := s.parent
		s.mu.Unlock()
		if runCtx == nil {
			return false
		}
		select {
		case <-space:
		case <-runCtx.Done():
			return false
		case <-parent.Done():
			return false
		}
	}
}

func (s *sess) emitRun(gen uint64, frames [][]byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done || gen != s.runGen {
		return false
	}
	for _, f := range frames {
		s.queueLocked(f)
	}
	return true
}

func (s *sess) endRun(gen uint64, inv, message string, finalText bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done || gen != s.runGen || s.runEnded {
		return false
	}
	s.runEnded = true
	if finalText {
		s.queueLocked(textFrame(inv, "", true))
	}
	s.queueLocked(endFrame(inv, message))
	return true
}

func (s *sess) queueLocked(b []byte) {
	s.frames = append(s.frames, b)
	close(s.note)
	s.note = make(chan struct{})
}

func (s *sess) popLocked() []byte {
	b := s.frames[0]
	s.frames = s.frames[1:]
	close(s.space)
	s.space = make(chan struct{})
	return b
}

func (s *sess) next(conn, watch context.Context) ([]byte, bool) {
	if watch == nil {
		watch = context.Background()
	}
	for {
		s.mu.Lock()
		if len(s.frames) > 0 {
			b := s.popLocked()
			s.mu.Unlock()
			return b, true
		}
		if s.done {
			s.mu.Unlock()
			return nil, false
		}
		note := s.note
		s.mu.Unlock()
		select {
		case <-conn.Done():
			return nil, false
		case <-watch.Done():
			return nil, false
		case <-note:
		}
	}
}

func (s *sess) finish() {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	s.done = true
	s.queueLocked(connectEnd())
	prev := s.runCancel
	s.runCancel = nil
	parent := s.parentCancel
	s.mu.Unlock()
	if prev != nil {
		prev()
	}
	if parent != nil {
		parent()
	}
	if s.hub != nil {
		s.hub.dropIfIdle(s)
	}
}

func (s *sess) shutdown() {
	s.mu.Lock()
	prev := s.runCancel
	parent := s.parentCancel
	watch := s.watchCancel
	s.runCancel = nil
	if !s.done {
		s.done = true
		s.finished = true
		if !s.decided {
			s.decided = true
			close(s.ready)
		}
		close(s.note)
		s.note = make(chan struct{})
		close(s.space)
		s.space = make(chan struct{})
	}
	s.mu.Unlock()
	if prev != nil {
		prev()
	}
	if parent != nil {
		parent()
	}
	if watch != nil {
		watch()
	}
}

func bidiLog(format string, args ...any) {
	Note(fmt.Sprintf(format, args...))
}

var tlsNoted struct {
	mu sync.Mutex
	at map[string]time.Time
}

func noteTLS(host string) {
	tlsNoted.mu.Lock()
	if tlsNoted.at == nil {
		tlsNoted.at = map[string]time.Time{}
	}
	if time.Since(tlsNoted.at[host]) < time.Minute {
		tlsNoted.mu.Unlock()
		return
	}
	tlsNoted.at[host] = time.Now()
	tlsNoted.mu.Unlock()
	Note("tls host=" + host)
}
