package cursoride

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatPiecesKeepToolDeltas(t *testing.T) {
	line := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Shell","arguments":"{\"command\":"}}]}}]}`
	p, ok := chatPieces(line)
	if !ok || len(p.tools) != 1 || p.tools[0].ID != "call_1" || p.tools[0].Name != "Shell" || p.tools[0].Args != `{"command":` {
		t.Fatalf("%+v ok=%v", p, ok)
	}
	seal, ok := chatPieces(`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
	if !ok || !seal.seal {
		t.Fatalf("seal %+v", seal)
	}
}

func TestInferenceToolFrameAndAgentExecCarryTheCommand(t *testing.T) {
	frame := toolCallFrame("inv-1", "call_1", "Shell", `{"command":"ls"}`, true)
	payload := connectPayloadsMust(t, frame)
	fs, err := parseFields(payload)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := parseFields(fieldBytes(fs, 3)[0])
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := fieldString(inv, 1); got != "inv-1" {
		t.Fatalf("invocation %q", got)
	}
	stream, err := parseFields(fieldBytes(inv, 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	part, err := parseFields(fieldBytes(stream, 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := fieldString(part, 1); got != "call_1" {
		t.Fatalf("id %q", got)
	}
	if got, _ := fieldString(part, 2); got != "Shell" {
		t.Fatalf("name %q", got)
	}
	if got, _ := fieldString(part, 3); got != `{"command":"ls"}` {
		t.Fatalf("args %q", got)
	}
	if v, ok := fieldUint(part, 4); !ok || v != 1 {
		t.Fatal("is_complete not set")
	}

	frames, ok := agentToolFrames("call_1", "Shell", `{"command":"ls"}`, 7)
	if !ok || len(frames) != 2 {
		t.Fatalf("frames %d ok=%v", len(frames), ok)
	}
	exec := connectPayloadsMust(t, frames[1])
	top, err := parseFields(exec)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := parseFields(fieldBytes(top, 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := fieldUint(msg, 1); !ok || n != 7 {
		t.Fatalf("exec id %d", n)
	}
	shell, err := parseFields(fieldBytes(msg, 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := fieldString(shell, 1); got != "ls" {
		t.Fatalf("command %q", got)
	}
	if _, ok := agentToolFrames("call_1", "Shell", `{`, 1); ok {
		t.Fatal("invalid arguments were a tool call")
	}
}

func TestAgentWriteFramesUseTheWriteArm(t *testing.T) {
	frames, ok := agentToolFrames("call_w", "Write", `{"path":"a.go","file_text":"package a"}`, 4)
	if !ok || len(frames) != 2 {
		t.Fatalf("frames %d ok=%v", len(frames), ok)
	}
	started := connectPayloadsMust(t, frames[0])
	top, err := parseFields(started)
	if err != nil {
		t.Fatal(err)
	}
	upd, err := parseFields(fieldBytes(top, 1)[0])
	if err != nil {
		t.Fatal(err)
	}
	call, err := parseFields(fieldBytes(upd, 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	tool, err := parseFields(fieldBytes(call, 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(fieldBytes(tool, 15)) != 0 {
		t.Fatal("Write started as MCP")
	}
	arms := fieldBytes(tool, 64)
	if len(arms) != 1 {
		t.Fatalf("write arm %d", len(arms))
	}
	writeCall, err := parseFields(arms[0])
	if err != nil {
		t.Fatal(err)
	}
	args, err := parseFields(fieldBytes(writeCall, 1)[0])
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := fieldString(args, 1); got != "a.go" {
		t.Fatalf("started path %q", got)
	}
	if got, _ := fieldString(args, 2); got != "package a" {
		t.Fatalf("started content %q", got)
	}

	exec := connectPayloadsMust(t, frames[1])
	msgTop, err := parseFields(exec)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := parseFields(fieldBytes(msgTop, 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	writes := fieldBytes(msg, 3)
	if len(writes) != 1 {
		t.Fatalf("exec write %d", len(writes))
	}
	body, err := parseFields(writes[0])
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := fieldString(body, 1); got != "a.go" {
		t.Fatalf("exec path %q", got)
	}
	if got, _ := fieldString(body, 2); got != "package a" {
		t.Fatalf("exec file_text %q", got)
	}
}

func TestCompileChatForwardsInvokeToolsAndOutputCap(t *testing.T) {
	var schema wbuf
	schema.msg(1, protoEntry("type", protoString("object")))
	var wrap wbuf
	wrap.msg(1, protoEntry("jsonSchema", protoStructValue([]byte(schema))))
	var tool wbuf
	tool.str(1, "Read")
	tool.str(2, "Read a file")
	tool.msg(3, []byte(wrap))
	var cfg wbuf
	cfg.u32(1, 4096)
	var req wbuf
	req.msg(1, textMsg(1, "hi"))
	req.msg(2, []byte(tool))
	req.msg(4, []byte(cfg))
	var inv wbuf
	inv.str(1, "inv-9")
	inv.msg(2, []byte(req))
	var client wbuf
	client.msg(2, []byte(inv))
	chat, err := CompileChat(Route{Model: "oppo/deepseek-flash", Fast: true, ContextTokens: 200_000}, "s", []byte(client))
	if err != nil {
		t.Fatal(err)
	}
	if chat.MaxTokens != 4096 || len(chat.Tools) != 1 || chat.Tools[0].Name != "Read" {
		t.Fatalf("%+v", chat)
	}
	body, err := chat.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"fast"`) || strings.Contains(string(body), "200000") {
		t.Fatalf("fast or context leaked into the chat body: %s", body)
	}
	var got struct {
		MaxTokens int `json:"max_tokens"`
		Tools     []struct {
			Function struct {
				Name       string         `json:"name"`
				Parameters map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if json.Unmarshal(body, &got) != nil || got.MaxTokens != 4096 || len(got.Tools) != 1 || got.Tools[0].Function.Name != "Read" || got.Tools[0].Function.Parameters["type"] != "object" {
		t.Fatalf("%s", body)
	}
}

func protoString(s string) []byte {
	var v wbuf
	v.str(3, s)
	return []byte(v)
}

func protoStructValue(st []byte) []byte {
	var v wbuf
	v.msg(5, st)
	return []byte(v)
}

func protoEntry(key string, val []byte) []byte {
	var e wbuf
	e.str(1, key)
	e.msg(2, val)
	return []byte(e)
}
