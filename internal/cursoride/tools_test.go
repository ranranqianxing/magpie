package cursoride

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStreamedArgsFillPiExecDespiteNullContent(t *testing.T) {
	// DeepSeek and OpenAI streams put content:null beside tool_calls, send
	// index as 0.0, and split arguments into JSON fragments. Dropping any
	// of those chunks seals a call whose string fields Cursor rejects as
	// undefined.
	lines := []string{
		`data: {"choices":[{"delta":{"content":null,"tool_calls":[{"index":0,"id":"call_r","type":"function","function":{"name":"Read","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"content":null,"tool_calls":[{"index":0.0,"function":{"arguments":"{\"path\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"notes.txt\"}"}}]}}]}`,
	}
	var asm toolAssembler
	var partials []string
	for _, line := range lines {
		p, ok := chatPieces(line)
		if !ok {
			t.Fatalf("dropped %s", line)
		}
		for _, d := range p.tools {
			if frame := agentPartialFromDelta(&asm, d); frame != nil {
				partials = append(partials, partialArgsText(t, frame))
			}
		}
	}
	if len(partials) < 2 || partials[0] != `{"path":` || partials[len(partials)-1] != `{"path":"notes.txt"}` {
		t.Fatalf("partials %q", partials)
	}
	sealed := asm.seal()
	if len(sealed) != 1 || sealed[0].name != "Read" || sealed[0].args != `{"path":"notes.txt"}` {
		t.Fatalf("sealed %+v", sealed)
	}
	frames, ok := agentToolFrames(sealed[0].id, sealed[0].name, sealed[0].args, 3)
	if !ok || len(frames) != 2 {
		t.Fatalf("frames %d ok=%v", len(frames), ok)
	}
	exec := execMessage(t, frames[1])
	if len(fieldBytes(exec, 7)) != 0 || len(fieldBytes(exec, 45)) != 1 {
		t.Fatalf("read exec fields legacy=%d pi=%d", len(fieldBytes(exec, 7)), len(fieldBytes(exec, 45)))
	}
	body := nested(t, exec, 45)
	if got, _ := fieldString(body, 1); got != "notes.txt" {
		t.Fatalf("path %q", got)
	}
	if _, ok := fieldString(body, 2); ok {
		t.Fatal("tool id landed in PiRead offset")
	}
}

func TestObjectArgumentsAreNotDropped(t *testing.T) {
	line := `data: {"choices":[{"delta":{"content":null,"tool_calls":[{"index":0,"id":"call_w","function":{"name":"Write","arguments":{"path":"notes.txt","file_text":"hello"}}}]}}]}`
	p, ok := chatPieces(line)
	if !ok || len(p.tools) != 1 || p.tools[0].Name != "Write" || p.tools[0].Args != `{"path":"notes.txt","file_text":"hello"}` {
		t.Fatalf("%+v ok=%v", p, ok)
	}
	frames, ok := agentToolFrames("call_w", "Write", p.tools[0].Args, 1)
	if !ok || len(frames) != 2 {
		t.Fatalf("frames %d ok=%v", len(frames), ok)
	}
	exec := execMessage(t, frames[1])
	if len(fieldBytes(exec, 3)) != 0 || len(fieldBytes(exec, 48)) != 1 {
		t.Fatalf("write exec legacy=%d pi=%d", len(fieldBytes(exec, 3)), len(fieldBytes(exec, 48)))
	}
	body := nested(t, exec, 48)
	if got, _ := fieldString(body, 1); got != "notes.txt" {
		t.Fatalf("path %q", got)
	}
	if got, _ := fieldString(body, 2); got != "hello" {
		t.Fatalf("content %q", got)
	}
	if got := partialArgsText(t, agentPartialFrame("call_w", "Write", p.tools[0].Args)); got != `{"content":"hello","path":"notes.txt"}` {
		t.Fatalf("partial %s", got)
	}
}

func TestComposerToolsCarryStringArgsOnPiArms(t *testing.T) {
	cases := []struct {
		name, args string
		card, exec int
		fields     map[int]string
	}{
		{"Shell", `{"command":"pwd"}`, 62, 46, map[int]string{1: "pwd"}},
		{"Read", `{"path":"notes.txt"}`, 61, 45, map[int]string{1: "notes.txt"}},
		{"Grep", `{"pattern":"foo","path":"src"}`, 65, 49, map[int]string{1: "foo", 2: "src"}},
		{"Glob", `{"glob_pattern":"*.go","target_directory":"src"}`, 66, 50, map[int]string{1: "*.go", 2: "src"}},
		{"Write", `{"path":"notes.txt","content":"hello"}`, 64, 48, map[int]string{1: "notes.txt", 2: "hello"}},
		{"Edit", `{"path":"a.go","old_string":"x","new_string":"y"}`, 63, 47, map[int]string{1: "a.go"}},
		{"LS", `{"path":"src"}`, 67, 51, map[int]string{1: "src"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frames, ok := agentToolFrames("call_"+tc.name, tc.name, tc.args, 2)
			if !ok || len(frames) != 2 {
				t.Fatalf("frames %d ok=%v", len(frames), ok)
			}
			tool := startedTool(t, frames[0])
			if len(fieldBytes(tool, tc.card)) != 1 {
				t.Fatalf("card %d missing", tc.card)
			}
			exec := execMessage(t, frames[1])
			if len(fieldBytes(exec, tc.exec)) != 1 {
				t.Fatalf("exec %d missing", tc.exec)
			}
			body := nested(t, exec, tc.exec)
			if tc.name == "Edit" {
				reps := fieldBytes(body, 2)
				if len(reps) != 1 {
					t.Fatalf("replacements %d", len(reps))
				}
				rep, err := parseFields(reps[0])
				if err != nil {
					t.Fatal(err)
				}
				if got, _ := fieldString(rep, 1); got != "x" {
					t.Fatalf("old %q", got)
				}
				if got, _ := fieldString(rep, 2); got != "y" {
					t.Fatalf("new %q", got)
				}
			}
			for n, want := range tc.fields {
				if got, _ := fieldString(body, n); got != want {
					t.Fatalf("field %d = %q, want %q", n, got, want)
				}
			}
		})
	}
}

func partialArgsText(t *testing.T, frame []byte) string {
	t.Helper()
	top, err := parseFields(connectPayloadsMust(t, frame))
	if err != nil {
		t.Fatal(err)
	}
	upd, err := parseFields(fieldBytes(top, 1)[0])
	if err != nil {
		t.Fatal(err)
	}
	part, err := parseFields(fieldBytes(upd, 7)[0])
	if err != nil {
		t.Fatal(err)
	}
	got, _ := fieldString(part, 3)
	return got
}

func execMessage(t *testing.T, frame []byte) []field {
	t.Helper()
	top, err := parseFields(connectPayloadsMust(t, frame))
	if err != nil {
		t.Fatal(err)
	}
	msg, err := parseFields(fieldBytes(top, 2)[0])
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func nested(t *testing.T, msg []field, n int) []field {
	t.Helper()
	body, err := parseFields(fieldBytes(msg, n)[0])
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func startedTool(t *testing.T, frame []byte) []field {
	t.Helper()
	top, err := parseFields(connectPayloadsMust(t, frame))
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
	return tool
}

func TestPartialWithoutANameDoesNotOpenAToolCard(t *testing.T) {
	var asm toolAssembler
	line := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"notes.txt\"}"}}]}}]}`
	p, ok := chatPieces(line)
	if !ok || len(p.tools) != 1 {
		t.Fatalf("%+v", p)
	}
	frame := agentPartialFromDelta(&asm, p.tools[0])
	top, err := parseFields(connectPayloadsMust(t, frame))
	if err != nil {
		t.Fatal(err)
	}
	upd, err := parseFields(fieldBytes(top, 1)[0])
	if err != nil {
		t.Fatal(err)
	}
	part, err := parseFields(fieldBytes(upd, 7)[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(fieldBytes(part, 2)) != 0 {
		t.Fatal("unnamed partial opened a tool card")
	}
	if got, _ := fieldString(part, 3); got != `{"path":"notes.txt"}` {
		t.Fatalf("args %q", got)
	}
}

func TestNonIntegerToolIndexIsNotMerged(t *testing.T) {
	line := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Read","arguments":"{\"path\":\"a\"}"}},{"index":1.5,"function":{"name":"Read","arguments":"{\"path\":\"b\"}"}}]}}]}`
	p, ok := chatPieces(line)
	if !ok || len(p.tools) != 1 || p.tools[0].Args != `{"path":"a"}` {
		t.Fatalf("%+v ok=%v", p, ok)
	}
}

func TestEmptyArgumentsDoNotHideInput(t *testing.T) {
	line := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"Read","arguments":"","input":{"path":"notes.txt"}}}]}}]}`
	p, ok := chatPieces(line)
	if !ok || len(p.tools) != 1 || p.tools[0].Args != `{"path":"notes.txt"}` {
		t.Fatalf("%+v ok=%v", p, ok)
	}
}

func TestFinishedArgsIgnoreALaterShorterCopy(t *testing.T) {
	var asm toolAssembler
	for _, args := range []string{`{"path":"notes.txt"}`, `{"path":`, `{}`} {
		p, ok := chatPieces(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"Read","arguments":` + jsonQuote(args) + `}}]}}]}`)
		if !ok || len(p.tools) != 1 {
			t.Fatalf("%s %+v", args, p)
		}
		agentPartialFromDelta(&asm, p.tools[0])
	}
	sealed := asm.seal()
	if len(sealed) != 1 || sealed[0].args != `{"path":"notes.txt"}` {
		t.Fatalf("%+v", sealed)
	}
}

func TestPlaceholderObjectDoesNotSwallowLaterFragments(t *testing.T) {
	// A relay sends "{}" with the tool name, then the real JSON as
	// fragments. Treating that object as finished drops the fragments,
	// and the Pi exec Cursor runs is Read {}.
	lines := []string{
		`data: {"choices":[{"delta":{"content":null,"tool_calls":[{"index":0,"id":"call_r","type":"function","function":{"name":"Read","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"content":null,"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"/tmp/magpie-p0-verify/README.md\"}"}}]}}]}`,
	}
	var asm toolAssembler
	var partials int
	for _, line := range lines {
		p, ok := chatPieces(line)
		if !ok || len(p.tools) != 1 {
			t.Fatalf("dropped %s", line)
		}
		if frame := agentPartialFromDelta(&asm, p.tools[0]); frame != nil {
			partials++
			if strings.TrimSpace(partialArgsText(t, frame)) == "{}" {
				t.Fatal("placeholder {} was published as a partial")
			}
		}
	}
	if partials == 0 {
		t.Fatal("no partial carried the real arguments")
	}
	sealed := asm.seal()
	if len(sealed) != 1 || sealed[0].args != `{"path":"/tmp/magpie-p0-verify/README.md"}` {
		t.Fatalf("sealed %+v", sealed)
	}
	frames, ok := agentToolFrames(sealed[0].id, sealed[0].name, sealed[0].args, 1)
	if !ok || len(frames) != 2 {
		t.Fatalf("frames %d ok=%v", len(frames), ok)
	}
	body := nested(t, execMessage(t, frames[1]), 45)
	if got, _ := fieldString(body, 1); got != "/tmp/magpie-p0-verify/README.md" {
		t.Fatalf("path %q", got)
	}
}

func TestPlaceholderFragmentsForComposerTools(t *testing.T) {
	cases := []struct {
		name, args string
		exec       int
		field      int
		want       string
	}{
		{"Shell", `{"command":"printf hi > notes.txt"}`, 46, 1, "printf hi > notes.txt"},
		{"Grep", `{"pattern":"foo","path":"src"}`, 49, 1, "foo"},
		{"Glob", `{"pattern":"*.md","path":"/tmp"}`, 50, 1, "*.md"},
		{"Write", `{"path":"notes.txt","content":"hello"}`, 48, 2, "hello"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mid := len(tc.args) / 2
			sealed := sealArgChunks(t, tc.name, "{}", tc.args[:mid], tc.args[mid:])
			if sealed.args != tc.args {
				t.Fatalf("sealed %s", sealed.args)
			}
			frames, ok := agentToolFrames(sealed.id, sealed.name, sealed.args, 1)
			if !ok || len(frames) != 2 {
				t.Fatalf("frames %d ok=%v", len(frames), ok)
			}
			body := nested(t, execMessage(t, frames[1]), tc.exec)
			if got, _ := fieldString(body, tc.field); got != tc.want {
				t.Fatalf("field %d %q", tc.field, got)
			}
		})
	}
}

func sealArgChunks(t *testing.T, name string, chunks ...string) toolBuf {
	t.Helper()
	var asm toolAssembler
	for _, args := range chunks {
		line := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"` + name + `","arguments":` + jsonQuote(args) + `}}]}}]}`
		p, ok := chatPieces(line)
		if !ok || len(p.tools) != 1 {
			t.Fatalf("dropped %s", line)
		}
		agentPartialFromDelta(&asm, p.tools[0])
	}
	sealed := asm.seal()
	if len(sealed) != 1 {
		t.Fatalf("%+v", sealed)
	}
	return sealed[0]
}

func TestFillEmptyToolArgsFromToolCallTag(t *testing.T) {
	calls := []toolBuf{{id: "c", name: "Shell", args: "{}"}}
	fillEmptyToolArgs(calls, `<tool_call>{"name":"Shell","arguments":{"command":"printf hi > notes.txt"}}</tool_call>`)
	if calls[0].args != `{"command":"printf hi > notes.txt"}` {
		t.Fatalf("%s", calls[0].args)
	}
	kept := []toolBuf{{name: "Read", args: `{"path":"a"}`}}
	fillEmptyToolArgs(kept, `<tool_call>{"name":"Read","arguments":{"path":"b"}}</tool_call>`)
	if kept[0].args != `{"path":"a"}` {
		t.Fatalf("overwrote %s", kept[0].args)
	}
	other := []toolBuf{{name: "Read", args: "{}"}}
	fillEmptyToolArgs(other, `<tool_call>{"name":"Shell","arguments":{"command":"ls"}}</tool_call>`)
	if other[0].args != "{}" {
		t.Fatalf("filled from another tool %s", other[0].args)
	}
	prose := []toolBuf{{name: "Read", args: "{}"}}
	fillEmptyToolArgs(prose, "I'll read /tmp/magpie-p0-verify/README.md")
	if prose[0].args != "{}" {
		t.Fatalf("prose became %s", prose[0].args)
	}
	alias := []toolBuf{{name: "Shell", args: "{}"}}
	fillEmptyToolArgs(alias, `<tool_call>{"name":"bash","arguments":{"command":"pwd"}}</tool_call>`)
	if alias[0].args != `{"command":"pwd"}` {
		t.Fatalf("alias %s", alias[0].args)
	}
}

func TestEmptyArgumentsObjectDoesNotHideInput(t *testing.T) {
	line := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"Read","arguments":"{}","input":{"path":"notes.txt"}}}]}}]}`
	p, ok := chatPieces(line)
	if !ok || len(p.tools) != 1 || p.tools[0].Args != `{"path":"notes.txt"}` {
		t.Fatalf("%+v ok=%v", p, ok)
	}
}

func TestEmptyObjectIsReplacedByTheRealArguments(t *testing.T) {
	var asm toolAssembler
	for _, args := range []string{`{}`, `{"path":"notes.txt"}`} {
		p, ok := chatPieces(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"Read","arguments":` + jsonQuote(args) + `}}]}}]}`)
		if !ok || len(p.tools) != 1 {
			t.Fatalf("%s %+v", args, p)
		}
		agentPartialFromDelta(&asm, p.tools[0])
	}
	sealed := asm.seal()
	if len(sealed) != 1 || sealed[0].args != `{"path":"notes.txt"}` {
		t.Fatalf("%+v", sealed)
	}
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestCumulativeArgSnapshotDoesNotDouble(t *testing.T) {
	var asm toolAssembler
	for _, line := range []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"Read","arguments":"{\"pa"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"notes.txt\"}"}}]}}]}`,
	} {
		p, ok := chatPieces(line)
		if !ok || len(p.tools) != 1 {
			t.Fatalf("%s %+v", line, p)
		}
		agentPartialFromDelta(&asm, p.tools[0])
	}
	sealed := asm.seal()
	if len(sealed) != 1 || sealed[0].args != `{"path":"notes.txt"}` {
		t.Fatalf("%+v", sealed)
	}
}

func TestEmptyWriteContentStaysAString(t *testing.T) {
	frames, ok := agentToolFrames("c", "Write", `{"path":"notes.txt","content":""}`, 1)
	if !ok || len(frames) != 2 {
		t.Fatalf("frames %d ok=%v", len(frames), ok)
	}
	body := nested(t, execMessage(t, frames[1]), 48)
	got, present := fieldString(body, 2)
	if !present || got != "" {
		t.Fatalf("content present=%v %q", present, got)
	}
}

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
	if len(fieldBytes(msg, 2)) != 0 {
		t.Fatal("shell exec used the legacy arm")
	}
	shell, err := parseFields(fieldBytes(msg, 46)[0])
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
	if len(fieldBytes(msg, 3)) != 0 {
		t.Fatal("write exec used the legacy arm")
	}
	writes := fieldBytes(msg, 48)
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
