package cursoride

import (
	"container/list"
	"crypto/sha256"
	"encoding/json"
	"sync"
)

// invokeCacheMax is how much marshaled gateway JSON the process keeps
// for BidiAppend messages it has already compiled. The key is the
// protobuf of one message, so a later tool round reuses the prefix
// instead of decoding its images and escaping it again. One message
// larger than the cap is not stored: it is compiled for that request
// and the prefix beside it can still hit.
const invokeCacheMax = 32 << 20

type invokeItem struct {
	key   [32]byte
	msgs  []ChatMessage
	wire  []json.RawMessage
	notes []string
	n     int
}

var invokeMu sync.Mutex
var invokeLRU = list.New()
var invokeMap = map[[32]byte]*list.Element{}
var invokeBytes int

// projectCached compiles one inference message, or returns the compile
// this process already did for the same bytes. hit is that reuse.
// Image notes from the first compile are repeated, so a cache hit still
// says which pictures were kept. The bytes of the picture are not logged.
func projectCached(raw []byte) (msgs []ChatMessage, wire []json.RawMessage, hit bool, err error) {
	sum := sha256.Sum256(raw)
	if item, ok := invokeGet(sum); ok {
		for _, n := range item.notes {
			Note(n)
		}
		return cloneMessages(item.msgs), item.wire, true, nil
	}
	var notes []string
	msgs, err = projectMessage(raw, &notes)
	if err != nil {
		return nil, nil, false, err
	}
	notes = append([]string(nil), notes...)
	// An empty message has no JSON. Caching it would not add to
	// invokeBytes, so the map would grow with nothing for the LRU to
	// drop. Parsing it again is the whole cost.
	if len(msgs) == 0 {
		return nil, nil, false, nil
	}
	w, werr := wireOf(msgs)
	if werr != nil {
		return msgs, nil, false, nil
	}
	n := 0
	for _, one := range w {
		n += len(one)
	}
	invokePut(invokeItem{key: sum, msgs: cloneMessages(msgs), wire: w, notes: notes, n: n})
	return cloneMessages(msgs), w, false, nil
}

func invokeGet(key [32]byte) (invokeItem, bool) {
	invokeMu.Lock()
	defer invokeMu.Unlock()
	el, ok := invokeMap[key]
	if !ok {
		return invokeItem{}, false
	}
	invokeLRU.MoveToFront(el)
	return el.Value.(invokeItem), true
}

func invokePut(item invokeItem) {
	if item.n > invokeCacheMax {
		return
	}
	invokeMu.Lock()
	defer invokeMu.Unlock()
	if el, ok := invokeMap[item.key]; ok {
		old := el.Value.(invokeItem)
		invokeBytes -= old.n
		el.Value = item
		invokeBytes += item.n
		invokeLRU.MoveToFront(el)
	} else {
		el := invokeLRU.PushFront(item)
		invokeMap[item.key] = el
		invokeBytes += item.n
	}
	for invokeBytes > invokeCacheMax && invokeLRU.Len() > 1 {
		back := invokeLRU.Back()
		old := back.Value.(invokeItem)
		delete(invokeMap, old.key)
		invokeLRU.Remove(back)
		invokeBytes -= old.n
	}
}

func resetInvokeCache() {
	invokeMu.Lock()
	defer invokeMu.Unlock()
	invokeLRU = list.New()
	invokeMap = map[[32]byte]*list.Element{}
	invokeBytes = 0
}

func cloneMessages(in []ChatMessage) []ChatMessage {
	if len(in) == 0 {
		return nil
	}
	out := make([]ChatMessage, len(in))
	for i, m := range in {
		out[i] = m
		if len(m.ToolCalls) > 0 {
			out[i].ToolCalls = append([]ToolCall(nil), m.ToolCalls...)
		}
		if parts, ok := m.Content.([]ContentPart); ok {
			copied := append([]ContentPart(nil), parts...)
			out[i].Content = copied
		}
	}
	return out
}
