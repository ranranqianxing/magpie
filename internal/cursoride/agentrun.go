package cursoride

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
)

// AgentTurn is one AgentService/Run client message, decoded far enough
// to decide whether the model is magpie's and to ask the gateway.
//
// ModelID is RequestedModel.model_id (field 9), or ModelDetails.model_id
// (field 3) when the run names no requested model. It is the catalog id
// the picker stored, including a variant string. Params are the
// RequestedModel parameters beside that id. Text is the user message on
// the action. Messages is that text plus the conversation_state prompt
// (root_prompt_messages_json) and the action's selected_context, resolved
// through pre_fetched_blobs.
// Tools is the composer tool list. ConversationID is the run's
// conversation, when it named one. MissingBlobs counts prompt ids the
// request did not carry bytes for.
type AgentTurn struct {
	ModelID        string
	Params         []Parameter
	ConversationID string
	Text           string
	Messages       []ChatMessage
	Tools          []ToolSpec
	MissingBlobs   int
}

// ReadAgentRun reads an AgentService/Run body. ok is false when the body
// has no run request, which includes a heartbeat and a request this
// package does not know. A connect frame sequence is walked; a body that
// is one protobuf is read as that message. A gzipped connect frame (flag
// bit 0) is decompressed. A trailer frame is skipped. A frame that does
// not parse is unknown, so ok is false and the caller forwards the body.
func ReadAgentRun(body []byte) (AgentTurn, bool, error) {
	payloads, framed := connectPayloads(body)
	if !framed {
		return readAgentClient(body)
	}
	var last error
	for _, p := range payloads {
		turn, ok, err := readAgentClient(p)
		if err != nil {
			last = err
			continue
		}
		if ok {
			return turn, true, nil
		}
	}
	if last != nil {
		return AgentTurn{}, false, last
	}
	return AgentTurn{}, false, nil
}

func readAgentClient(payload []byte) (AgentTurn, bool, error) {
	fs, err := parseFields(payload)
	if err != nil {
		return AgentTurn{}, false, nil
	}
	runs := fieldBytes(fs, 1)
	if len(runs) == 0 {
		return AgentTurn{}, false, nil
	}
	rf, err := parseFields(runs[0])
	if err != nil {
		return AgentTurn{}, false, err
	}
	id, params := "", []Parameter(nil)
	if ms := fieldBytes(rf, 9); len(ms) > 0 {
		id, params = readRequestedModel(ms[len(ms)-1])
	}
	if id == "" {
		if ms := fieldBytes(rf, 3); len(ms) > 0 {
			id, _ = readRequestedModel(ms[len(ms)-1])
		}
	}
	if id == "" {
		return AgentTurn{}, false, nil
	}
	conv, _ := fieldString(rf, 5)
	text := readAgentText(rf)
	blobs := blobIndex(fieldBytes(rf, 17))
	var msgs []ChatMessage
	var missing int
	if states := fieldBytes(rf, 1); len(states) > 0 {
		msgs, missing, err = replayConversation(states[0], blobs)
		if err != nil {
			return AgentTurn{}, false, err
		}
	}
	attach := readUserAttach(rf, blobs)
	missing += attach.missing
	if attach.skipped {
		Note("agent attach skipped reason=context")
	} else if n := len(attach.images); n > 0 || len(attach.pieces) > 0 {
		Note(fmt.Sprintf("agent attach offered images=%d texts=%d", n, len(attach.pieces)))
	}
	return AgentTurn{
		ModelID: id, Params: params, ConversationID: conv,
		Text: text, Messages: appendCurrent(msgs, text, attach),
		Tools: agentTools(rf), MissingBlobs: missing,
	}, true, nil
}

func readRequestedModel(msg []byte) (string, []Parameter) {
	fs, err := parseFields(msg)
	if err != nil {
		return "", nil
	}
	id, _ := fieldString(fs, 1)
	var params []Parameter
	for _, raw := range fieldBytes(fs, 3) {
		pf, err := parseFields(raw)
		if err != nil {
			continue
		}
		k, _ := fieldString(pf, 1)
		v, _ := fieldString(pf, 2)
		if k == "" {
			continue
		}
		params = append(params, Parameter{ID: k, Value: v})
	}
	return id, params
}

func readAgentText(run []field) string {
	mf, ok := readUserMessage(run)
	if !ok {
		return ""
	}
	text, _ := fieldString(mf, 1)
	return text
}

// readUserMessage is AgentRunRequest.action.user_message. The action is
// field 2, UserMessageAction.user_message is field 1, and that message's
// text is field 1. A missing or truncated step is no message.
func readUserMessage(run []field) ([]field, bool) {
	actions := fieldBytes(run, 2)
	if len(actions) == 0 {
		return nil, false
	}
	af, err := parseFields(actions[0])
	if err != nil {
		return nil, false
	}
	umas := fieldBytes(af, 1)
	if len(umas) == 0 {
		return nil, false
	}
	uf, err := parseFields(umas[0])
	if err != nil {
		return nil, false
	}
	ums := fieldBytes(uf, 1)
	if len(ums) == 0 {
		return nil, false
	}
	mf, err := parseFields(ums[0])
	if err != nil {
		return nil, false
	}
	return mf, true
}

// connectPayloads is the protobuf inside each connect data frame. framed
// is false when body is not a whole sequence of frames, so the caller
// reads body itself. Flag bit 0 is gzip. Flag bit 1 is a trailer.
func connectPayloads(body []byte) (payloads [][]byte, framed bool) {
	if len(body) < 5 || body[0] > 3 {
		return nil, false
	}
	b := body
	for len(b) > 0 {
		if len(b) < 5 {
			return nil, false
		}
		flags := b[0]
		if flags > 3 {
			return nil, false
		}
		n := binary.BigEndian.Uint32(b[1:5])
		if uint64(n) > uint64(len(b)-5) {
			return nil, false
		}
		payload := b[5 : 5+int(n)]
		b = b[5+int(n):]
		if flags&0x02 != 0 {
			continue
		}
		if flags&0x01 != 0 {
			plain, err := gunzipBytes(payload)
			if err != nil {
				return nil, false
			}
			payload = plain
		}
		payloads = append(payloads, payload)
	}
	if len(payloads) == 0 {
		return nil, false
	}
	return payloads, true
}

func gunzipBytes(b []byte) ([]byte, error) {
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
		return nil, errf("agent frame too large")
	}
	return plain, nil
}

// agentTextFrame is AgentServerMessage.interaction_update.text_delta.
func agentTextFrame(text string) []byte {
	var delta wbuf
	delta.str(1, text)
	var upd wbuf
	upd.msg(1, []byte(delta))
	var server wbuf
	server.msg(1, []byte(upd))
	return connectFrame([]byte(server))
}

// agentThinkingFrame is interaction_update.thinking_delta.
func agentThinkingFrame(text string) []byte {
	var delta wbuf
	delta.str(1, text)
	var upd wbuf
	upd.msg(4, []byte(delta))
	var server wbuf
	server.msg(1, []byte(upd))
	return connectFrame([]byte(server))
}

// agentTurnEnded is interaction_update.turn_ended. Cursor's composer
// stays on the turn until this frame arrives.
func agentTurnEnded() []byte {
	var upd wbuf
	upd.msg(14, nil)
	var server wbuf
	server.msg(1, []byte(upd))
	return connectFrame([]byte(server))
}
