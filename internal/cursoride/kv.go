package cursoride

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// The KV facade is how a long Agent session keeps the same prompt an
// official model sees. root_prompt_messages_json, the summary, and a
// turn the journal stored by id are 32-byte sha256 blobs. The run
// prefetches some of them. The rest stay on the client. Cursor's agent
// server asks for those with KvServerMessage.get_blob_args before it
// calls the model. An injected model goes through this proxy, so the
// proxy asks the same way, on the same AgentService/Run stream, and
// replays the root prompt with the bytes that come back.
//
// Field numbers are the agent protocol this package already speaks:
// AgentServerMessage field 4 is kv_server_message, AgentClientMessage
// field 3 is kv_client_message (field 2 is the exec result, field 5 is
// the exec control). KvServerMessage is id=1, get_blob_args=2.
// GetBlobArgs.blob_id and GetBlobResult.blob_data are field 1.
// A buffered run never asks: the body is already finished, and waiting
// would stall a client that cannot answer.

const (
	kvHydrateBatch  = 64
	kvHydrateRounds = 8
	kvHydrateMax    = 512
	// maxKVBlob matches the client's own per-blob cap. A larger payload
	// is a miss, not a prompt.
	maxKVBlob = 16 << 20
	// kvHydrateIdle is silence after the gets were written. A client
	// that speaks KV answers from local storage inside this window.
	// kvHydrateWait is the whole exchange, so a drip cannot hold the turn.
	kvHydrateIdle = time.Second
	kvHydrateWait = 4 * time.Second
)

// followSource is the request frames still to be read. Hydrate may see
// a frame that is not a KV reply; that frame stays in front for the
// tool loop.
type followSource struct {
	ch  <-chan []byte
	buf [][]byte
}

func (s *followSource) take() ([]byte, bool) {
	if s == nil {
		return nil, false
	}
	if len(s.buf) > 0 {
		p := s.buf[0]
		s.buf = s.buf[1:]
		return p, true
	}
	if s.ch == nil {
		return nil, false
	}
	p, ok := <-s.ch
	return p, ok
}

func (s *followSource) takeUntil(ctx context.Context, idle, overall <-chan time.Time) ([]byte, bool) {
	if s == nil {
		return nil, false
	}
	if len(s.buf) > 0 {
		p := s.buf[0]
		s.buf = s.buf[1:]
		return p, true
	}
	if s.ch == nil {
		return nil, false
	}
	select {
	case p, ok := <-s.ch:
		return p, ok
	case <-idle:
		return nil, false
	case <-overall:
		return nil, false
	case <-ctx.Done():
		return nil, false
	}
}

func (s *followSource) prepend(frames [][]byte) {
	if s == nil || len(frames) == 0 {
		return
	}
	s.buf = append(append([][]byte(nil), frames...), s.buf...)
}

// kvGetFrame is one GetBlobArgs on the agent response stream.
func kvGetFrame(id uint32, blobID []byte) []byte {
	var args wbuf
	args.msg(1, blobID)
	var kv wbuf
	kv.u32(1, id)
	kv.msg(2, []byte(args))
	var server wbuf
	server.msg(4, []byte(kv))
	return connectFrame([]byte(server))
}

// classifyClientFrame reports what one AgentClientMessage is.
// kv-get carries GetBlobResult (data may be empty: the client has no
// blob). kv-other is a set ack. heartbeat is the run keepalive.
func classifyClientFrame(payload []byte) (kind string, id uint32, data []byte) {
	fs, err := parseFields(payload)
	if err != nil {
		return "other", 0, nil
	}
	if hasField(fs, 7) && !hasField(fs, 1) && !hasField(fs, 2) && !hasField(fs, 3) && !hasField(fs, 5) {
		return "heartbeat", 0, nil
	}
	raws := fieldBytes(fs, 3)
	if len(raws) == 0 {
		return "other", 0, nil
	}
	kf, err := parseFields(raws[0])
	if err != nil {
		return "other", 0, nil
	}
	n, _ := fieldUint(kf, 1)
	gets := fieldBytes(kf, 2)
	if len(gets) == 0 {
		return "kv-other", uint32(n), nil
	}
	gf, err := parseFields(gets[0])
	if err != nil {
		return "kv-get", uint32(n), nil
	}
	if b := fieldBytes(gf, 1); len(b) > 0 {
		data = b[0]
	}
	return "kv-get", uint32(n), data
}

// hydrateAgentTurn asks for blob ids the run did not prefetch and
// rebuilds the prompt from what the client returns. A cancelled context,
// a client that does not answer, or a blob that is not a message leaves
// the prompt that was already decoded. Turns stay the stand-in in that case.
func hydrateAgentTurn(ctx context.Context, turn AgentTurn, write func([]byte), src *followSource) AgentTurn {
	if !turn.liveKV || write == nil || src == nil || len(turn.rawRun) == 0 {
		return turn
	}
	if err := ctx.Err(); err != nil {
		return turn
	}
	original := turn
	overall := time.NewTimer(kvHydrateWait)
	defer overall.Stop()
	skip := map[string]bool{}
	var requested, filled, skipped int
	for round := 0; round < kvHydrateRounds && filled < kvHydrateMax; round++ {
		if err := ctx.Err(); err != nil {
			break
		}
		ids := missingBlobIDs(turn.rawRun, turn.blobs, skip)
		if len(ids) == 0 {
			break
		}
		room := kvHydrateMax - filled
		if len(ids) > kvHydrateBatch {
			ids = ids[:kvHydrateBatch]
		}
		if len(ids) > room {
			ids = ids[:room]
		}
		pending := make(map[uint32][]byte, len(ids))
		var n uint32 = 1
		for _, id := range ids {
			wireID := canonicalBlobID(id)
			if wireID == nil {
				continue
			}
			pending[n] = append([]byte(nil), id...)
			write(kvGetFrame(n, wireID))
			requested++
			n++
		}
		if len(pending) == 0 {
			break
		}
		idle := time.NewTimer(kvHydrateIdle)
		var held [][]byte
		got := 0
		for len(pending) > 0 {
			payload, ok := src.takeUntil(ctx, idle.C, overall.C)
			if !ok {
				break
			}
			kind, id, data := classifyClientFrame(payload)
			switch kind {
			case "heartbeat", "kv-other":
				continue
			case "kv-get":
				rawID, want := pending[id]
				if !want {
					continue
				}
				delete(pending, id)
				markSkipped := func() {
					if c := canonicalBlobID(rawID); c != nil {
						skip[hex.EncodeToString(c)] = true
					}
					skipped++
				}
				if !blobUsable(data) {
					markSkipped()
					resetIdle(idle, kvHydrateIdle)
					continue
				}
				if turn.blobs == nil {
					turn.blobs = map[string][]byte{}
				}
				stored := append([]byte(nil), data...)
				storeBlob(turn.blobs, rawID, stored)
				filled++
				got++
				resetIdle(idle, kvHydrateIdle)
			default:
				held = append(held, payload)
			}
		}
		idle.Stop()
		src.prepend(held)
		if got == 0 {
			break
		}
	}
	if filled == 0 {
		if requested > 0 {
			Note(fmt.Sprintf("agent kv hydrate requested=%d filled=0 skipped=%d blobs-missing=%d", requested, skipped, original.MissingBlobs))
		}
		return original
	}
	if err := turn.rebuild(false); err != nil {
		Note("agent kv hydrate fail")
		return original
	}
	Note(fmt.Sprintf("agent kv hydrate requested=%d filled=%d skipped=%d messages=%d blobs-missing=%d images=%d",
		requested, filled, skipped, len(turn.Messages), turn.MissingBlobs, countChatImages(turn.Messages)))
	return turn
}

func resetIdle(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

func storeBlob(dst map[string][]byte, rawID, data []byte) {
	putBlob(dst, rawID, data)
	if c := canonicalBlobID(rawID); c != nil && !bytes.Equal(c, rawID) {
		putBlob(dst, c, data)
	}
}

// blobUsable is a prompt blob. JSON that is not a message is refused:
// one broken root entry must not drop the siblings that parsed.
// Anything else (a turn protobuf, a summary, an image) is kept.
func blobUsable(data []byte) bool {
	if len(data) == 0 || len(data) > maxKVBlob {
		return false
	}
	if !looksJSON(data) {
		return true
	}
	_, err := messagesFromJSON(data)
	return err == nil
}

// canonicalBlobID is the 32-byte sha256 the client stored. The state
// may carry those bytes, or the same id as 64 hex characters.
func canonicalBlobID(raw []byte) []byte {
	if looksJSON(raw) || len(raw) == 0 {
		return nil
	}
	if len(raw) == 32 {
		return raw
	}
	if !utf8.Valid(raw) {
		return nil
	}
	s := strings.TrimSpace(string(raw))
	if len(s) != 64 {
		return nil
	}
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return nil
	}
	return b
}

// missingBlobIDs is every blob the replay would still miss, root prompt
// first, then the summary and the current turn's context, then the
// journal. The journal is walked only while the root still has a hole
// or is empty: a complete root prompt does not need the turns copy.
func missingBlobIDs(run []byte, blobs map[string][]byte, skip map[string]bool) [][]byte {
	rf, err := parseFields(run)
	if err != nil {
		return nil
	}
	var ids [][]byte
	seen := map[string]bool{}
	add := func(raw []byte) {
		c := canonicalBlobID(raw)
		if c == nil {
			return
		}
		key := hex.EncodeToString(c)
		if skip[key] || seen[key] {
			return
		}
		if _, miss := resolveBlob(raw, blobs); miss == 0 {
			return
		}
		seen[key] = true
		ids = append(ids, append([]byte(nil), raw...))
	}
	var rootHoles int
	var rootEntries int
	if states := fieldBytes(rf, 1); len(states) > 0 {
		rootHoles, rootEntries = walkStateBlobs(states[0], blobs, add)
	}
	if msg, ok := readUserMessage(rf); ok {
		walkUserBlobs(msg, blobs, add)
	}
	if states := fieldBytes(rf, 1); len(states) > 0 && (rootHoles > 0 || rootEntries == 0) {
		walkTurnBlobs(states[0], blobs, add)
	}
	return ids
}

func walkStateBlobs(state []byte, blobs map[string][]byte, add func([]byte)) (holes, entries int) {
	fs, err := parseFields(state)
	if err != nil {
		return 0, 0
	}
	for _, raw := range fieldBytes(fs, 1) {
		entries++
		if canonicalBlobID(raw) == nil {
			continue
		}
		if _, miss := resolveBlob(raw, blobs); miss > 0 {
			holes++
			add(raw)
		}
	}
	if raw := fieldBytes(fs, 6); len(raw) > 0 {
		add(raw[0])
	}
	return holes, entries
}

func walkTurnBlobs(state []byte, blobs map[string][]byte, add func([]byte)) {
	fs, err := parseFields(state)
	if err != nil {
		return
	}
	for _, raw := range fieldBytes(fs, 8) {
		walkTurnBlob(raw, blobs, add)
	}
}

func walkTurnBlob(raw []byte, blobs map[string][]byte, add func([]byte)) {
	if body, ok := resolvedBlob(raw, blobs, add); ok {
		raw = body
	} else if canonicalBlobID(raw) != nil {
		return
	}
	if looksJSON(raw) {
		return
	}
	fs, err := parseFields(raw)
	if err != nil {
		return
	}
	agents := fieldBytes(fs, 1)
	if len(agents) == 0 {
		return
	}
	walkAgentTurn(agents[0], blobs, add)
}

func walkAgentTurn(raw []byte, blobs map[string][]byte, add func([]byte)) {
	if body, ok := resolvedBlob(raw, blobs, add); ok {
		raw = body
	} else if canonicalBlobID(raw) != nil {
		return
	}
	af, err := parseFields(raw)
	if err != nil {
		return
	}
	for _, u := range fieldBytes(af, 1) {
		walkUserStep(u, blobs, add)
	}
	for _, s := range fieldBytes(af, 2) {
		walkStepBlob(s, blobs, add)
	}
}

func walkUserStep(raw []byte, blobs map[string][]byte, add func([]byte)) {
	if body, ok := resolvedBlob(raw, blobs, add); ok {
		raw = body
	} else if canonicalBlobID(raw) != nil {
		return
	}
	uf, err := parseFields(raw)
	if err != nil {
		return
	}
	if ctx := fieldBytes(uf, 3); len(ctx) > 0 {
		walkContextBlobs(ctx[0], add)
	}
}

func walkStepBlob(raw []byte, blobs map[string][]byte, add func([]byte)) {
	if _, ok := resolvedBlob(raw, blobs, add); ok {
		return
	}
	if canonicalBlobID(raw) != nil {
		return
	}
}

// resolvedBlob adds raw when it is a blob id still missing. ok means
// raw is now the blob bytes and the caller should walk those.
func resolvedBlob(raw []byte, blobs map[string][]byte, add func([]byte)) ([]byte, bool) {
	if canonicalBlobID(raw) == nil {
		return nil, false
	}
	body, miss := resolveBlob(raw, blobs)
	if miss > 0 || len(body) == 0 {
		add(raw)
		return nil, false
	}
	return body, true
}

func walkUserBlobs(msg []field, blobs map[string][]byte, add func([]byte)) {
	if raw := fieldBytes(msg, 3); len(raw) > 0 {
		walkContextBlobs(raw[0], add)
	}
	raws := fieldBytes(msg, 10)
	if len(raws) == 0 {
		return
	}
	raw := raws[0]
	if body, ok := resolvedBlob(raw, blobs, add); ok {
		walkContextBlobs(body, add)
		return
	}
	if canonicalBlobID(raw) != nil {
		return
	}
	walkContextBlobs(raw, add)
}

func walkContextBlobs(raw []byte, add func([]byte)) {
	fs, err := parseFields(raw)
	if err != nil {
		return
	}
	for _, img := range fieldBytes(fs, 1) {
		walkImageBlob(img, add)
	}
	for _, ent := range fieldBytes(fs, 16) {
		ef, err := parseFields(ent)
		if err != nil {
			continue
		}
		if ids := fieldBytes(ef, 2); len(ids) > 0 {
			add(ids[0])
		}
	}
}

func walkImageBlob(raw []byte, add func([]byte)) {
	fs, err := parseFields(raw)
	if err != nil {
		return
	}
	if len(fieldBytes(fs, 8)) > 0 {
		return
	}
	if wrapped := fieldBytes(fs, 9); len(wrapped) > 0 {
		wf, err := parseFields(wrapped[0])
		if err != nil {
			return
		}
		if len(fieldBytes(wf, 2)) > 0 {
			return
		}
		if ids := fieldBytes(wf, 1); len(ids) > 0 {
			add(ids[0])
		}
		return
	}
	if ids := fieldBytes(fs, 1); len(ids) > 0 {
		add(ids[0])
	}
}
