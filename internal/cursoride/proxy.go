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
	p.hub = newHub()
	p.client = &http.Client{Transport: &http.Transport{
		// A nil Proxy would also skip HTTP_PROXY. Set it so a later edit
		// cannot inherit ProxyFromEnvironment and send Cursor's traffic
		// back into this proxy.
		Proxy:               func(*http.Request) (*url.URL, error) { return nil, nil },
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
	}}
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
	p.ln, p.srv = nil, nil
	p.mu.Unlock()
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
	w.Header().Set("Content-Type", "application/connect+proto")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	for {
		frame, ok := s.next(r.Context())
		if !ok {
			return
		}
		_, _ = w.Write(frame)
		if fl != nil {
			fl.Flush()
		}
	}
}

func (p *Proxy) bidi(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(maxCatalogBody)+1))
	if err != nil || len(body) > maxCatalogBody {
		bidiLog("read-fail err=%v len=%d enc=%q", err, len(body), r.Header.Get("Content-Encoding"))
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	body, gunzipped := gunzipRequest(body, r.Header.Get("Content-Encoding"))
	if gunzipped {
		bidiLog("gunzipped len=%d", len(body))
	}
	plain, _, err := UnwrapUnary(body)
	if err != nil {
		bidiLog("unwrap-fail err=%v len=%d", err, len(body))
		http.Error(w, "append", http.StatusBadRequest)
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
	case data != "":
		raw, err = hex.DecodeString(data)
		if err != nil {
			bidiLog("hex-fail err=%v dataLen=%d", err, len(data))
			http.Error(w, "append", http.StatusBadRequest)
			return
		}
		bidiLog("append id=%s via=hex n=%d", id, len(raw))
	default:
		bidiLog("forward empty-payload id=%s", id)
		p.forward(w, r, body, gunzipped)
		return
	}
	if s := p.hub.localOf(id); s != nil {
		p.localAppend(w, s, raw)
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

func (p *Proxy) localAppend(w http.ResponseWriter, s *sess, raw []byte) {
	// Cursor 3.x sometimes nests a Connect frame inside BidiAppend data.
	if plain, framed, uerr := UnwrapUnary(raw); uerr == nil && framed && len(plain) > 0 {
		bidiLog("localAppend unwrap nested connect n=%d->%d", len(raw), len(plain))
		raw = plain
	}
	c, err := ReadClient(raw)
	if err != nil {
		bidiLog("ReadClient-fail n=%d err=%v", len(raw), err)
		http.Error(w, "append", http.StatusBadRequest)
		return
	}
	switch c.Kind {
	case KindInvoke:
		chat, err := CompileChat(s.route, s.conv, raw)
		if err != nil {
			id := c.InvocationID
			if id == "" {
				id = "invoke"
			}
			bidiLog("CompileChat-fail inv=%q err=%v", id, err)
			s.push(invocationIdFrame(id))
			s.push(endFrame(id, err.Error()))
		} else {
			go p.runChat(s, chat)
		}
	case KindCancel:
		s.stop()
	case KindFinish:
		s.finish()
	}
	writeProto(w, nil, "application/proto")
}

func (p *Proxy) runChat(s *sess, chat Chat) {
	Note("chat start model=" + chat.Model + " path=BidiAppend")
	defer Note("chat done model=" + chat.Model + " path=BidiAppend")
	noteRequestToolArgs(chat.Messages)
	s.push(invocationIdFrame(chat.InvocationID))
	body, err := chat.JSON()
	if err != nil {
		s.push(endFrame(chat.InvocationID, "cursor ide: chat"))
		return
	}
	base, token := p.Gateway()
	req, err := http.NewRequestWithContext(s.context(), http.MethodPost, strings.TrimRight(base, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		s.push(endFrame(chat.InvocationID, "cursor ide: gateway"))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range chat.Header() {
		req.Header.Set(k, v)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		s.push(endFrame(chat.InvocationID, "cursor ide: gateway"))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.push(endFrame(chat.InvocationID, fmt.Sprintf("cursor ide: gateway status %d", resp.StatusCode)))
		return
	}
	var sawText bool
	var asm toolAssembler
	var said strings.Builder
	eachPiece(resp.Body, func(piece chatPiece) {
		if piece.thinking != "" {
			s.push(thinkingFrame(chat.InvocationID, piece.thinking))
			said.WriteString(piece.thinking)
			said.WriteByte('\n')
		}
		// Cursor drops text on is_final and only emits finish; content
		// must ride non-final text_part frames (#Cursor 3.27 local loop).
		if piece.text != "" {
			sawText = true
			s.push(textFrame(chat.InvocationID, piece.text, false))
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
	})
	finishInferenceTools(s, chat.InvocationID, &asm, said.String())
	if sawText {
		s.push(textFrame(chat.InvocationID, "", true))
	}
	s.push(endFrame(chat.InvocationID, ""))
}

// finishInferenceTools writes one completing tool_call_part per call that
// Pi can run. The part's arguments are the sealed JSON, not the unsent
// tail of a fragment. Cursor's local loop parses that part and runs the
// tool. A call that is still missing its strings is not sent.
func finishInferenceTools(s *sess, invocation string, asm *toolAssembler, text string) {
	markers := markupMarkers(text)
	run := sealRound(asm, text, func(msg string) {
		s.push(textFrame(invocation, msg, false))
	})
	for _, t := range run {
		s.push(toolCallFrame(invocation, t.id, t.name, displayArgs(t.name, t.args), true))
		noteToolTrace("frames", t.name, t.args, t.frags, t.placeholders, t.fill, markers, "written")
	}
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
	if err != nil {
		Note("agent-run history fail")
		http.Error(w, "history", http.StatusBadRequest)
		return
	}
	if !ok {
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
	fr := newConnectFrameReader(bodyReader)
	first, err := fr.next()
	if err != nil {
		http.Error(w, "run", http.StatusBadRequest)
		return
	}
	forwardStreaming := func() {
		p.forward(w, r, streamingAgentForwardBody(first, fr), gunzipped)
	}
	turn, ok, err := readAgentClient(first)
	if err != nil {
		Note("agent-run history fail")
		http.Error(w, "history", http.StatusBadRequest)
		return
	}
	if !ok {
		Note("agent-run forward reason=not-a-run")
		forwardStreaming()
		return
	}
	route, ours, err := RouteModel(p.Models(), turn.ModelID, turn.Params)
	if err != nil {
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

func streamingAgentForwardBody(first []byte, fr *connectFrameReader) []byte {
	return append(connectFrame(first), fr.readRemainder()...)
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
	if len(turn.Messages) == 0 {
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
	Note("chat start model=" + route.Model + " path=AgentService/Run")
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

func eachPiece(r io.Reader, fn func(chatPiece)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		piece, ok := chatPieces(sc.Text())
		if !ok {
			continue
		}
		fn(piece)
		if piece.done {
			return
		}
	}
}

func (p *Proxy) fetch(r *http.Request, body []byte) (int, []byte, string, error) {
	resp, err := p.do(r, body, false)
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
	enc := strings.ToLower(contentEncoding)
	if enc != "gzip" && !(len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b) {
		return b, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	plain, err := io.ReadAll(io.LimitReader(zr, int64(maxCatalogBody)+1))
	_ = zr.Close()
	if err != nil {
		return nil, err
	}
	if len(plain) > maxCatalogBody {
		return nil, errf("upstream catalog too large")
	}
	return plain, nil
}

// gunzipRequest is the body to inspect. plain is true only when those
// bytes are no longer gzip: a later forward must drop Content-Encoding
// and send Content-Length for this slice. A failed gunzip keeps the
// original bytes and plain false, so the headers the client sent still
// match. x-gzip is the same encoding.
func gunzipRequest(body []byte, contentEncoding string) ([]byte, bool) {
	enc := strings.ToLower(strings.TrimSpace(contentEncoding))
	magic := len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b
	if enc != "gzip" && enc != "x-gzip" && !magic {
		return body, false
	}
	plain, err := decodeCatalogBody(body, contentEncoding)
	if err != nil || bytes.Equal(plain, body) {
		if err != nil {
			bidiLog("gunzip-fail err=%v len=%d", err, len(body))
		}
		return body, false
	}
	return plain, true
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, body []byte, plain bool) {
	if body == nil {
		b, err := io.ReadAll(io.LimitReader(r.Body, int64(maxCatalogBody)+1))
		if err != nil || len(b) > maxCatalogBody {
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
	for k, vs := range resp.Header {
		if hop(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *Proxy) do(r *http.Request, body []byte, plain bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, p.origin(r), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// body is what is sent. The client's Content-Length is the size before
	// a gunzip, so it is not copied: the Transport writes this field.
	req.ContentLength = int64(len(body))
	for k, vs := range r.Header {
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
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	if plain {
		req.Header.Del("Content-Encoding")
	}
	return p.client.Do(req)
}

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

func decodeAppend(plain []byte) (id, data string, blob []byte, err error) {
	fs, err := parseFields(plain)
	if err != nil {
		return "", "", nil, err
	}
	data, _ = fieldString(fs, 1)
	if bins := fieldBytes(fs, 4); len(bins) > 0 && len(bins[0]) > 0 {
		blob = bins[0]
	}
	if ids := fieldBytes(fs, 2); len(ids) > 0 {
		inner, err := parseFields(ids[0])
		if err != nil {
			return "", "", nil, err
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

type hub struct {
	mu sync.Mutex
	m  map[string]*sess
}

func newHub() *hub { return &hub{m: map[string]*sess{}} }

func (h *hub) sess(id string) *sess {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.m[id]; s != nil {
		return s
	}
	s := &sess{ready: make(chan struct{}), note: make(chan struct{})}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	h.m[id] = s
	return s
}

func (h *hub) mark(id string, local bool, route Route, conv string) *sess {
	s := h.sess(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.decided {
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

type sess struct {
	mu       sync.Mutex
	local    bool
	decided  bool
	ready    chan struct{}
	route    Route
	conv     string
	ctx      context.Context
	cancel   context.CancelFunc
	frames   [][]byte
	note     chan struct{}
	done     bool
	finished bool
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

func (s *sess) context() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctx
}

func (s *sess) push(b []byte) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return
	}
	s.frames = append(s.frames, b)
	close(s.note)
	s.note = make(chan struct{})
	s.mu.Unlock()
}

func (s *sess) next(ctx context.Context) ([]byte, bool) {
	for {
		s.mu.Lock()
		if len(s.frames) > 0 {
			b := s.frames[0]
			s.frames = s.frames[1:]
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
		case <-ctx.Done():
			return nil, false
		case <-note:
		}
	}
}

func (s *sess) stop() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *sess) finish() {
	s.mu.Lock()
	if s.finished {
		s.mu.Unlock()
		return
	}
	s.finished = true
	s.frames = append(s.frames, connectEnd())
	s.done = true
	close(s.note)
	s.note = make(chan struct{})
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
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
