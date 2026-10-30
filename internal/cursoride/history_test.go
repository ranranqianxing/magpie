package cursoride

import (
	"strings"
	"testing"
)

func TestReplayConversationRestoresTurnsAndSkipsTheCurrentDuplicate(t *testing.T) {
	user := []byte(`{"role":"user","content":[{"type":"text","text":"<user_info>\nOS\n</user_info>\n<user_query>\nfirst\n</user_query>"}]}`)
	asst := []byte(`{"role":"assistant","content":[{"type":"text","text":"earlier"},{"type":"tool-call","toolCallId":"c1","toolName":"Read","args":{"path":"a.go"}}]}`)
	result := []byte(`{"role":"tool","content":[{"type":"tool-result","toolCallId":"c1","toolName":"Read","result":"package a"}]}`)
	var state wbuf
	state.msg(1, user)
	state.msg(1, asst)
	state.msg(1, result)
	msgs, missing, err := replayConversation([]byte(state), nil)
	if err != nil || missing != 0 {
		t.Fatalf("replay err=%v missing=%d", err, missing)
	}
	msgs = appendCurrent(msgs, "second", userAttach{})
	if len(msgs) != 4 {
		t.Fatalf("messages %d %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || !strings.Contains(msgs[0].Content.(string), "OS") || !strings.Contains(msgs[0].Content.(string), "first") || strings.Contains(msgs[0].Content.(string), "user_query") {
		t.Fatalf("user %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Content.(string) != "earlier" || len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].Name != "Read" || msgs[1].ToolCalls[0].Arguments != `{"path":"a.go"}` {
		t.Fatalf("assistant %+v", msgs[1])
	}
	if msgs[2].Role != "tool" || msgs[2].ToolCallID != "c1" || msgs[2].Content.(string) != "package a" {
		t.Fatalf("tool %+v", msgs[2])
	}
	if msgs[3].Role != "user" || msgs[3].Content.(string) != "second" {
		t.Fatalf("current %+v", msgs[3])
	}
	again := appendCurrent(msgs[:1], "first", userAttach{})
	if len(again) != 1 {
		t.Fatalf("duplicated the current prompt: %+v", again)
	}
}

func TestReplayConversationResolvesPrefetchedBlobs(t *testing.T) {
	id := make([]byte, 32)
	for i := range id {
		id[i] = byte(i + 1)
	}
	blob := []byte(`{"role":"assistant","content":"from blob"}`)
	var entry wbuf
	entry.msg(1, id)
	entry.msg(2, blob)
	var state wbuf
	state.msg(1, id)
	var requested wbuf
	requested.str(1, "codex/gpt-5.6-luna")
	var user wbuf
	user.str(1, "now")
	var action wbuf
	action.msg(1, []byte(user))
	var uma wbuf
	uma.msg(1, []byte(action))
	var full wbuf
	full.msg(1, []byte(state))
	full.msg(2, []byte(uma))
	full.str(5, "conv-1")
	full.msg(9, []byte(requested))
	full.msg(17, []byte(entry))
	got, ok, err := readAgentClient(agentClient([]byte(full)))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.MissingBlobs != 0 || len(got.Messages) != 2 {
		t.Fatalf("missing=%d messages=%d %+v", got.MissingBlobs, len(got.Messages), got.Messages)
	}
	if got.Messages[0].Role != "assistant" || got.Messages[0].Content.(string) != "from blob" {
		t.Fatalf("blob message %+v", got.Messages[0])
	}
	if got.Messages[1].Content.(string) != "now" {
		t.Fatalf("current %+v", got.Messages[1])
	}
	if len(got.Tools) < 6 {
		t.Fatalf("tools %d, want the built-in composer set", len(got.Tools))
	}
}

func TestReplayConversationRefusesATruncatedMessage(t *testing.T) {
	// Field 1 claims five bytes and then the buffer ends.
	_, _, err := replayConversation([]byte{0x0a, 0x05, 0x01}, nil)
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("err=%v", err)
	}
}

func TestReplayConversationRefusesInvalidJSON(t *testing.T) {
	var state wbuf
	state.msg(1, []byte(`{"role"`))
	_, _, err := replayConversation([]byte(state), nil)
	if err == nil {
		t.Fatal("invalid json was an empty history")
	}
}

func TestReplayConversationCountsABlobTheRequestDidNotCarry(t *testing.T) {
	id := make([]byte, 32)
	id[0] = 9
	var state wbuf
	state.msg(1, id)
	msgs, missing, err := replayConversation([]byte(state), nil)
	if err != nil || missing != 1 || len(msgs) != 0 {
		t.Fatalf("msgs=%d missing=%d err=%v", len(msgs), missing, err)
	}
}
