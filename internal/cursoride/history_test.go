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

func TestReplayToolCallUsesInputWhenArgsAreEmpty(t *testing.T) {
	input := []byte(`{"role":"assistant","content":[{"type":"tool-call","toolCallId":"c1","toolName":"Read","input":{"path":"a.go"}}]}`)
	msgs, err := messagesFromJSON(input)
	if err != nil || len(msgs) != 1 || len(msgs[0].ToolCalls) != 1 || msgs[0].ToolCalls[0].Arguments != `{"path":"a.go"}` {
		t.Fatalf("%v %+v", err, msgs)
	}
	both := []byte(`{"role":"assistant","content":[{"type":"tool-call","toolCallId":"c2","toolName":"Read","args":{},"input":{"path":"b.go"}}]}`)
	msgs, err = messagesFromJSON(both)
	if err != nil || len(msgs) != 1 || msgs[0].ToolCalls[0].Arguments != `{"path":"b.go"}` {
		t.Fatalf("args {} hid input: %v %+v", err, msgs)
	}
	const secret = "sk-live-history-value"
	empty := []byte(`{"role":"assistant","content":[{"type":"tool-call","toolCallId":"c3","toolName":"Read","note":"` + secret + `"}]}`)
	msgs, err = messagesFromJSON(empty)
	if err != nil || len(msgs) != 1 || msgs[0].ToolCalls[0].Arguments != "{}" {
		t.Fatalf("empty part: %v %+v", err, msgs)
	}
	var line string
	for _, one := range strings.Split(Recent(), "\n") {
		if strings.Contains(one, "agent history tool name=Read empty=true") && strings.Contains(one, "keys=") {
			line = one
		}
	}
	if line == "" || strings.Contains(line, secret) || !strings.Contains(line, "from=none keys=note,toolCallId,toolName,type") {
		t.Fatalf("history trace %q", line)
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

func TestReplayToolMessageKeepsItsCallIDAndOutput(t *testing.T) {
	byID, err := messagesFromJSON([]byte(`{"role":"tool","id":"c9","content":"package a"}`))
	if err != nil || len(byID) != 1 || byID[0].Role != "tool" || byID[0].ToolCallID != "c9" || byID[0].Content.(string) != "package a" {
		t.Fatalf("id: %v %+v", err, byID)
	}
	byOutput, err := messagesFromJSON([]byte(`{"role":"tool","content":[{"type":"tool-result","toolCallId":"c1","toolName":"Read","output":"package b"}]}`))
	if err != nil || len(byOutput) != 1 || byOutput[0].ToolCallID != "c1" || byOutput[0].Content.(string) != "package b" {
		t.Fatalf("output: %v %+v", err, byOutput)
	}
	withImage, err := messagesFromJSON([]byte(`{"role":"tool","content":[{"type":"tool-result","toolCallId":"c2","result":[{"type":"text","text":"see"},{"type":"image","image":"data:image/png;base64,aGk=","mediaType":"image/png"}]}]}`))
	if err != nil || len(withImage) != 1 {
		t.Fatalf("image result: %v %+v", err, withImage)
	}
	parts, ok := withImage[0].Content.([]ContentPart)
	if !ok || len(parts) != 2 || parts[0].Text != "see" || parts[1].ImageURL != "data:image/png;base64,aGk=" {
		t.Fatalf("image parts %#v", withImage[0].Content)
	}
}

func TestHistoryImageURLStringAndObjectStayImages(t *testing.T) {
	url, err := messagesFromJSON([]byte(`{"role":"user","content":[{"type":"text","text":"see"},{"type":"image_url","image_url":"data:image/png;base64,aGk="}]}`))
	if err != nil || len(url) != 1 {
		t.Fatalf("string image_url: %v %+v", err, url)
	}
	parts, ok := url[0].Content.([]ContentPart)
	if !ok || len(parts) != 2 || parts[1].ImageURL != "data:image/png;base64,aGk=" {
		t.Fatalf("string image %#v", url[0].Content)
	}
	obj, err := messagesFromJSON([]byte(`{"role":"user","content":[{"type":"image","image":{"data":"aGk=","mediaType":"image/png"}}]}`))
	if err != nil || len(obj) != 1 {
		t.Fatalf("object image: %v %+v", err, obj)
	}
	parts, ok = obj[0].Content.([]ContentPart)
	if !ok || len(parts) != 1 || parts[0].ImageURL != "data:image/png;base64,aGk=" {
		t.Fatalf("object image %#v", obj[0].Content)
	}
}

func TestReplayTurnsFillAMissingRootPrompt(t *testing.T) {
	id := make([]byte, 32)
	id[0] = 9
	var state wbuf
	state.msg(1, id)
	state.msg(8, agentTurnWithRead("fix the handler", "a.go", "c1", "package a"))
	msgs, missing, err := replayConversation([]byte(state), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) < 3 {
		t.Fatalf("messages %d missing=%d %+v", len(msgs), missing, msgs)
	}
	if msgs[0].Role != "user" || msgs[0].Content.(string) != "fix the handler" {
		t.Fatalf("user %+v", msgs[0])
	}
	if msgs[1].Role != "assistant" || len(msgs[1].ToolCalls) != 1 || msgs[1].ToolCalls[0].Name != "Read" || !strings.Contains(msgs[1].ToolCalls[0].Arguments, "a.go") {
		t.Fatalf("call %+v", msgs[1])
	}
	if msgs[2].Role != "tool" || msgs[2].ToolCallID != "c1" || msgs[2].Content.(string) != "package a" {
		t.Fatalf("result %+v", msgs[2])
	}
	if !strings.Contains(Recent(), "agent history source=turns") {
		t.Fatalf("trace:\n%s", Recent())
	}
}

func TestReplayTurnsDoNotReplaceACompleteRoot(t *testing.T) {
	var state wbuf
	state.msg(1, []byte(`{"role":"user","content":"from-root"}`))
	state.msg(8, agentTurnWithRead("from-turn", "a.go", "c1", "package a"))
	msgs, missing, err := replayConversation([]byte(state), nil)
	if err != nil || missing != 0 || len(msgs) != 1 || msgs[0].Content.(string) != "from-root" {
		t.Fatalf("missing=%d %+v err=%v", missing, msgs, err)
	}
}

func agentTurnWithRead(user, path, callID, output string) []byte {
	var um wbuf
	um.str(1, user)
	var args wbuf
	args.str(1, path)
	var success wbuf
	success.str(1, output)
	var result wbuf
	result.msg(1, []byte(success))
	var pi wbuf
	pi.msg(1, []byte(args))
	pi.msg(2, []byte(result))
	var call wbuf
	call.str(57, callID)
	call.msg(61, []byte(pi))
	var step wbuf
	step.msg(2, []byte(call))
	var agent wbuf
	agent.msg(1, []byte(um))
	agent.msg(2, []byte(step))
	var turn wbuf
	turn.msg(1, []byte(agent))
	return []byte(turn)
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
