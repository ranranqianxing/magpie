package cursoride

import (
	"bufio"
	"bytes"
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
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = p.srv.Serve(ln) }()
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
	switch r.URL.Path {
	case PathAvailableModels, PathUsableModels, PathUsableModelsAI, PathDefaultModel, PathDefaultModelCLI, PathDefaultModelNudge:
		(&Handler{Models: p.Models, Upstream: p.fetch}).ServeHTTP(w, r)
	case "/aiserver.v1.NetworkService/IsConnected":
		writeProto(w, nil, "application/proto")
	case "/aiserver.v1.InferenceService/RunInferenceSSE":
		p.inferenceSSE(w, r)
	case "/aiserver.v1.BidiService/BidiAppend":
		p.bidi(w, r)
	default:
		p.forward(w, r, nil)
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
		p.forward(w, r.WithContext(r.Context()), body)
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
		http.Error(w, "read", http.StatusBadRequest)
		return
	}
	plain, _, err := UnwrapUnary(body)
	if err != nil {
		http.Error(w, "append", http.StatusBadRequest)
		return
	}
	id, data, blob, err := decodeAppend(plain)
	if err != nil {
		http.Error(w, "append", http.StatusBadRequest)
		return
	}
	if len(blob) > 0 || data == "" || id == "" {
		p.forward(w, r, body)
		return
	}
	raw, err := hex.DecodeString(data)
	if err != nil {
		http.Error(w, "append", http.StatusBadRequest)
		return
	}
	if s := p.hub.localOf(id); s != nil {
		p.localAppend(w, s, raw)
		return
	}
	if run, ok := inferenceRun(raw); ok {
		route, ours, err := RouteModel(p.Models(), run.ModelID, run.Params)
		if err != nil {
			http.Error(w, "model", http.StatusBadRequest)
			return
		}
		if !ours {
			p.hub.mark(id, false, Route{}, "")
			p.forward(w, r, body)
			return
		}
		s := p.hub.mark(id, true, route, run.ConversationID)
		s.push(runReady(route.Model, route.Effort, route.ContextTokens))
		writeProto(w, nil, "application/proto")
		return
	}
	p.forward(w, r, body)
}

func (p *Proxy) localAppend(w http.ResponseWriter, s *sess, raw []byte) {
	c, err := ReadClient(raw)
	if err != nil {
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
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var held string
	for sc.Scan() {
		piece, ok := chatPieces(sc.Text())
		if !ok {
			continue
		}
		if piece.done {
			break
		}
		if piece.thinking != "" {
			s.push(thinkingFrame(chat.InvocationID, piece.thinking))
		}
		if piece.text != "" {
			if held != "" {
				s.push(textFrame(chat.InvocationID, held, false))
			}
			held = piece.text
		}
	}
	if held != "" {
		s.push(textFrame(chat.InvocationID, held, true))
	}
	s.push(endFrame(chat.InvocationID, ""))
}

func (p *Proxy) fetch(r *http.Request, body []byte) (int, []byte, string, error) {
	resp, err := p.do(r, body)
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
	return resp.StatusCode, b, resp.Header.Get("Content-Type"), nil
}

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, body []byte) {
	if body == nil {
		b, err := io.ReadAll(io.LimitReader(r.Body, int64(maxCatalogBody)+1))
		if err != nil || len(b) > maxCatalogBody {
			http.Error(w, "read", http.StatusBadRequest)
			return
		}
		body = b
	}
	resp, err := p.do(r, body)
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

func (p *Proxy) do(r *http.Request, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, p.origin(r), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, vs := range r.Header {
		if hop(k) || strings.EqualFold(k, "Host") {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
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
