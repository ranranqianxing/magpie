package cursoride

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"strings"
)

// ToolSpec is one tool the gateway chat body offers the model.
// Parameters is a JSON Schema object. Provider is set for an MCP tool
// Cursor named on the run; built-in tools leave it empty.
type ToolSpec struct {
	Name        string
	Description string
	Provider    string
	Parameters  json.RawMessage
}

// toolDelta is one streamed tool_calls fragment from a chat completion.
type toolDelta struct {
	Index int
	ID    string
	Name  string
	Args  string
}

// toolBuf accumulates one call. emitted is how much of Args was already
// handed to Cursor as a delta, so a later frame sends only the new tail.
type toolBuf struct {
	id, name, args string
	emitted        int
	closed         bool
}

func (t *toolBuf) take() string {
	if t.emitted >= len(t.args) {
		return ""
	}
	frag := t.args[t.emitted:]
	t.emitted = len(t.args)
	return frag
}

// appendArgs folds one arguments chunk. A later chunk that already starts
// with the buffer is the aggregated JSON so far (some streams resend it);
// anything else is the next fragment.
func (t *toolBuf) appendArgs(s string) {
	if s == "" {
		return
	}
	if t.args != "" && strings.HasPrefix(s, t.args) {
		t.args = s
		if t.emitted > len(t.args) {
			t.emitted = len(t.args)
		}
		return
	}
	// The arguments are already one JSON object. A later longer object
	// replaces it ({} then the real call). Anything else, including a
	// resend of the opening bytes, would glue on and fail json.Valid.
	if argsReady(t.args) {
		if argsReady(s) && len(s) > len(t.args) {
			t.args = s
			t.emitted = len(t.args)
		}
		return
	}
	t.args += s
}

// toolAssembler folds streaming tool deltas and closes them once.
type toolAssembler struct {
	by    map[int]*toolBuf
	order []int
}

func (a *toolAssembler) add(d toolDelta) (id, name, frag string) {
	if a.by == nil {
		a.by = map[int]*toolBuf{}
	}
	t := a.by[d.Index]
	if t == nil {
		t = &toolBuf{}
		a.by[d.Index] = t
		a.order = append(a.order, d.Index)
	}
	if d.ID != "" {
		t.id = d.ID
	}
	if d.Name != "" {
		t.name = d.Name
	}
	if d.Args != "" {
		t.appendArgs(d.Args)
	}
	if t.id == "" {
		t.id = "call-" + itoa(d.Index+1)
	}
	return t.id, t.name, t.take()
}

// agentPartialFromDelta records one gateway fragment and returns the
// partial_tool_call frame. The frame's arguments text is the aggregated
// JSON so far. Cursor reads that field as a snapshot, so a bare fragment
// leaves the IDE validating whatever tail arrived last.
func agentPartialFromDelta(asm *toolAssembler, d toolDelta) []byte {
	if asm == nil {
		return nil
	}
	id, name, frag := asm.add(d)
	if frag == "" {
		return nil
	}
	full := frag
	if asm.by != nil && asm.by[d.Index] != nil && asm.by[d.Index].args != "" {
		full = asm.by[d.Index].args
	}
	return agentPartialFrame(id, name, full)
}

// seal closes every call that is still open and returns each one's full
// arguments. A second seal is empty.
func (a *toolAssembler) seal() []toolBuf {
	var out []toolBuf
	for _, i := range a.order {
		t := a.by[i]
		if t == nil || t.closed {
			continue
		}
		t.closed = true
		out = append(out, *t)
	}
	return out
}

// agentTools is what an Agent composer run offers the model: Cursor's
// built-in tools, then any MCP tool the run defined. The inference path
// does not use this list; it forwards only the tools on the invoke.
func agentTools(run []field) []ToolSpec {
	out := append([]ToolSpec{}, builtinAgentTools()...)
	seen := map[string]bool{}
	for _, t := range out {
		seen[strings.ToLower(t.Name)] = true
	}
	for _, block := range fieldBytes(run, 4) {
		fs, err := parseFields(block)
		if err != nil {
			continue
		}
		for _, raw := range fieldBytes(fs, 1) {
			spec, ok := mcpTool(raw)
			if !ok || seen[strings.ToLower(spec.Name)] {
				continue
			}
			seen[strings.ToLower(spec.Name)] = true
			out = append(out, spec)
		}
	}
	return out
}

func builtinAgentTools() []ToolSpec {
	return []ToolSpec{
		{Name: "Shell", Description: "Run a shell command in the workspace.", Parameters: schema(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)},
		{Name: "Read", Description: "Read a file.", Parameters: schema(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)},
		{Name: "Grep", Description: "Search files for a pattern.", Parameters: schema(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"glob":{"type":"string"}},"required":["pattern"]}`)},
		{Name: "Delete", Description: "Delete a file.", Parameters: schema(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)},
		{Name: "Glob", Description: "Find files by a glob pattern.", Parameters: schema(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"}},"required":["pattern"]}`)},
		{Name: "LS", Description: "List a directory.", Parameters: schema(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
		{Name: "Edit", Description: "Replace text in a file.", Parameters: schema(`{"type":"object","properties":{"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"}},"required":["path","old_string","new_string"]}`)},
		{Name: "Write", Description: "Write a file.", Parameters: schema(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`)},
	}
}

func schema(s string) json.RawMessage {
	return json.RawMessage(s)
}

func mcpTool(raw []byte) (ToolSpec, bool) {
	fs, err := parseFields(raw)
	if err != nil {
		return ToolSpec{}, false
	}
	name, _ := fieldString(fs, 5)
	if name == "" {
		name, _ = fieldString(fs, 1)
	}
	if name == "" {
		return ToolSpec{}, false
	}
	desc, _ := fieldString(fs, 2)
	provider, _ := fieldString(fs, 4)
	params := json.RawMessage(`{"type":"object","properties":{}}`)
	if js, ok := fieldString(fs, 6); ok && json.Valid([]byte(js)) {
		params = json.RawMessage(js)
	} else if blobs := fieldBytes(fs, 3); len(blobs) > 0 && looksJSON(blobs[0]) && json.Valid(blobs[0]) {
		params = json.RawMessage(blobs[0])
	}
	return ToolSpec{Name: name, Description: desc, Provider: provider, Parameters: params}, true
}

// inferenceTools reads InferenceStreamRequest.tools (field 2). The
// parameters struct is Cursor's {jsonSchema: schema} wrap. A tool with
// no name is skipped. A struct that does not parse is an error.
func inferenceTools(req []field) ([]ToolSpec, error) {
	var out []ToolSpec
	for _, raw := range fieldBytes(req, 2) {
		fs, err := parseFields(raw)
		if err != nil {
			return nil, err
		}
		name, _ := fieldString(fs, 1)
		if name == "" {
			continue
		}
		desc, _ := fieldString(fs, 2)
		params := json.RawMessage(`{"type":"object","properties":{}}`)
		if blobs := fieldBytes(fs, 3); len(blobs) > 0 {
			obj, err := decodeStruct(blobs[0])
			if err != nil {
				return nil, err
			}
			if schema, ok := obj["jsonSchema"]; ok {
				b, err := json.Marshal(schema)
				if err != nil {
					return nil, err
				}
				params = b
			} else if len(obj) > 0 {
				b, err := json.Marshal(obj)
				if err != nil {
					return nil, err
				}
				params = b
			}
		}
		out = append(out, ToolSpec{Name: name, Description: desc, Parameters: params})
	}
	return out, nil
}

func decodeStruct(b []byte) (map[string]any, error) {
	fs, err := parseFields(b)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, raw := range fieldBytes(fs, 1) {
		ef, err := parseFields(raw)
		if err != nil {
			return nil, err
		}
		k, _ := fieldString(ef, 1)
		vs := fieldBytes(ef, 2)
		if k == "" || len(vs) == 0 {
			continue
		}
		v, err := decodeValue(vs[0])
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

func decodeValue(b []byte) (any, error) {
	fs, err := parseFields(b)
	if err != nil {
		return nil, err
	}
	for _, f := range fs {
		switch f.num {
		case 1:
			return nil, nil
		case 2:
			if len(f.b) == 8 {
				return math.Float64frombits(binary.LittleEndian.Uint64(f.b)), nil
			}
		case 3:
			return string(f.b), nil
		case 4:
			return f.v != 0, nil
		case 5:
			return decodeStruct(f.b)
		case 6:
			inner, err := parseFields(f.b)
			if err != nil {
				return nil, err
			}
			var list []any
			for _, one := range fieldBytes(inner, 1) {
				v, err := decodeValue(one)
				if err != nil {
					return nil, err
				}
				list = append(list, v)
			}
			return list, nil
		}
	}
	return nil, nil
}

// toolCallFrame is InferenceStreamResponse.tool_call_part. args is the
// fragment Cursor concatenates until is_complete.
func toolCallFrame(invocation, id, name, args string, complete bool) []byte {
	var part wbuf
	part.str(1, id)
	part.str(2, name)
	part.str(3, args)
	if complete {
		part.bool(4, true)
	}
	var stream wbuf
	stream.msg(2, []byte(part))
	return invocationFrame(invocation, []byte(stream))
}

// agentPartialFrame is interaction_update.partial_tool_call. args is the
// aggregated arguments JSON so far (PartialToolCallUpdate.args_text_delta),
// not only the newest fragment. When that JSON is an object, the tool
// card carries the same strings the exec will.
func agentPartialFrame(id, name, args string) []byte {
	text := displayArgs(name, args)
	var part wbuf
	part.str(1, id)
	// An unfinished fragment has no string fields yet. Attaching an empty
	// tool body makes Cursor validate those strings as undefined and run
	// the tool again. The card waits until the JSON is an object and the
	// call has a name: a nameless body is the MCP arm.
	if argsReady(text) && strings.TrimSpace(name) != "" {
		if call := agentToolCall(id, name, text, true); len(call) > 0 {
			part.msg(2, call)
		}
	}
	part.str(3, text)
	part.str(4, id)
	var upd wbuf
	upd.msg(7, []byte(part))
	var server wbuf
	server.msg(1, []byte(upd))
	return connectFrame([]byte(server))
}

// agentToolFrames is the pair Cursor's composer runs: tool_call_started
// for the card, then exec_server_message so the IDE executes it. ok is
// false when args is not JSON, which is not a call to run.
func agentToolFrames(id, name, args string, execN uint32) (frames [][]byte, ok bool) {
	if !json.Valid([]byte(args)) {
		return nil, false
	}
	var started wbuf
	started.str(1, id)
	started.msg(2, agentToolCall(id, name, args, true))
	started.str(3, id)
	var upd wbuf
	upd.msg(2, []byte(started))
	var server wbuf
	server.msg(1, []byte(upd))
	frames = append(frames, connectFrame([]byte(server)))
	if exec := agentExec(id, name, args, execN); len(exec) > 0 {
		var msg wbuf
		msg.msg(2, exec)
		frames = append(frames, connectFrame([]byte(msg)))
	}
	return frames, true
}

func agentToolCall(id, name, args string, full bool) []byte {
	var tool wbuf
	tool.str(57, id)
	kind, body := toolBody(id, name, args, full)
	if kind == 0 {
		return []byte(tool)
	}
	tool.msg(kind, body)
	return []byte(tool)
}

// toolBody is the ToolCall oneof arm. full is set once arguments are a
// JSON object. A partial frame calls this only then, so the card opens
// with the strings Cursor validates.
func toolBody(id, name, args string, full bool) (int, []byte) {
	obj := map[string]any{}
	if full {
		_ = json.Unmarshal([]byte(args), &obj)
	}
	if card, _, pi, ok := piFrame(name, obj); ok {
		var call wbuf
		call.msg(1, pi)
		return card, []byte(call)
	}
	switch canonTool(name) {
	case "delete":
		var del wbuf
		putArg(&del, 1, obj, "path", "file_path")
		var call wbuf
		call.msg(1, []byte(del))
		return 3, []byte(call)
	default:
		var mcp wbuf
		mcp.str(1, name)
		mcp.str(3, id)
		mcp.str(5, name)
		for k, v := range obj {
			var entry wbuf
			entry.str(1, k)
			entry.msg(2, protoValue(v))
			mcp.msg(2, []byte(entry))
		}
		var call wbuf
		call.msg(1, []byte(mcp))
		return 15, []byte(call)
	}
}

func agentExec(id, name, args string, n uint32) []byte {
	if n == 0 {
		n = 1
	}
	var obj map[string]any
	if json.Unmarshal([]byte(args), &obj) != nil {
		return nil
	}
	var exec wbuf
	exec.u32(1, n)
	exec.str(15, id)
	if _, field, pi, ok := piFrame(name, obj); ok {
		exec.msg(field, pi)
		return []byte(exec)
	}
	switch canonTool(name) {
	case "delete":
		var d wbuf
		putArg(&d, 1, obj, "path", "file_path")
		d.str(2, id)
		exec.msg(4, []byte(d))
	default:
		var mcp wbuf
		mcp.str(1, name)
		mcp.str(3, id)
		mcp.str(5, name)
		for k, v := range obj {
			var entry wbuf
			entry.str(1, k)
			entry.msg(2, protoValue(v))
			mcp.msg(2, []byte(entry))
		}
		exec.msg(11, []byte(mcp))
	}
	return []byte(exec)
}

func canonTool(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "shell", "bash", "run_terminal_cmd", "terminal":
		return "shell"
	case "read", "read_file":
		return "read"
	case "grep", "ripgrep":
		return "grep"
	case "delete", "delete_file":
		return "delete"
	case "ls", "list_dir", "list_directory":
		return "ls"
	case "glob", "find", "glob_file_search", "file_search":
		return "glob"
	case "edit", "strreplace", "str_replace", "search_replace":
		return "edit"
	case "write", "write_file":
		return "write"
	default:
		return ""
	}
}

// piFrame is the ToolCall card and ExecServerMessage arm Cursor 3.27
// runs. The legacy read/shell/grep/write arms are a different oneof;
// validators on the Pi tools never see those strings.
//
// Cards: read 61, shell 62, edit 63, write 64, grep 65, glob 66, ls 67.
// Exec:  read 45, shell 46, edit 47, write 48, grep 49, glob 50, ls 51.
func piFrame(name string, obj map[string]any) (card, exec int, args []byte, ok bool) {
	switch canonTool(name) {
	case "shell":
		return 62, 46, bashPi(obj), true
	case "read":
		return 61, 45, readPi(obj), true
	case "grep":
		return 65, 49, grepPi(obj), true
	case "glob":
		return 66, 50, findPi(obj), true
	case "ls":
		return 67, 51, lsPi(obj), true
	case "write":
		return 64, 48, writePi(obj), true
	case "edit":
		if editAsWrite(obj) {
			return 64, 48, writePi(obj), true
		}
		return 63, 47, editPi(obj), true
	default:
		return 0, 0, nil, false
	}
}

func bashPi(obj map[string]any) []byte {
	var b wbuf
	putArg(&b, 1, obj, "command", "cmd")
	return []byte(b)
}

func readPi(obj map[string]any) []byte {
	var b wbuf
	putArg(&b, 1, obj, "path", "file_path", "target_file")
	putI32(&b, 2, obj, "offset")
	putI32(&b, 3, obj, "limit")
	return []byte(b)
}

func grepPi(obj map[string]any) []byte {
	var b wbuf
	putArg(&b, 1, obj, "pattern", "query")
	putArg(&b, 2, obj, "path", "file_path")
	putArg(&b, 3, obj, "glob", "glob_pattern")
	return []byte(b)
}

func findPi(obj map[string]any) []byte {
	var b wbuf
	putArg(&b, 1, obj, "pattern", "glob_pattern", "glob", "query")
	putArg(&b, 2, obj, "path", "target_directory")
	return []byte(b)
}

func lsPi(obj map[string]any) []byte {
	var b wbuf
	putArg(&b, 1, obj, "path", "target_directory")
	return []byte(b)
}

func writePi(obj map[string]any) []byte {
	var b wbuf
	putArg(&b, 1, obj, "path", "file_path")
	putArg(&b, 2, obj, "content", "contents", "file_text", "stream_content")
	return []byte(b)
}

func editPi(obj map[string]any) []byte {
	var rep wbuf
	putArg(&rep, 1, obj, "old_string", "oldText", "old_str")
	if _, ok := argPresent(obj, "new_string", "newText", "new_str"); ok {
		putArg(&rep, 2, obj, "new_string", "newText", "new_str")
	} else {
		putArg(&rep, 2, obj, "stream_content")
	}
	var b wbuf
	putArg(&b, 1, obj, "path", "file_path")
	if len(rep) > 0 {
		b.msg(2, []byte(rep))
	}
	return []byte(b)
}

func editAsWrite(obj map[string]any) bool {
	if argString(obj, "old_string", "oldText", "old_str", "new_string", "newText", "new_str") != "" {
		return false
	}
	return argString(obj, "content", "contents", "file_text", "stream_content") != ""
}

func putArg(b *wbuf, field int, obj map[string]any, keys ...string) {
	if s, ok := argPresent(obj, keys...); ok {
		b.str(field, s)
	}
}

func argPresent(obj map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if _, ok := obj[k]; !ok || obj[k] == nil {
			continue
		}
		return argString(obj, k), true
	}
	return "", false
}

func putI32(b *wbuf, field int, obj map[string]any, keys ...string) {
	for _, k := range keys {
		v, ok := obj[k]
		if !ok || v == nil {
			continue
		}
		switch n := v.(type) {
		case float64:
			if n < 0 || n > math.MaxInt32 || n != math.Trunc(n) {
				return
			}
			b.u32(field, uint32(n))
			return
		case int:
			if n < 0 || n > math.MaxInt32 {
				return
			}
			b.u32(field, uint32(n))
			return
		case json.Number:
			i, err := n.Int64()
			if err != nil || i < 0 || i > math.MaxInt32 {
				return
			}
			b.u32(field, uint32(i))
			return
		}
	}
}

func argsReady(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "{") && json.Valid([]byte(s))
}

// displayArgs rewrites a finished arguments object onto the names the Pi
// tools validate (content, pattern, path). An unfinished fragment is
// returned as itself.
func displayArgs(name, raw string) string {
	if !argsReady(raw) {
		return raw
	}
	var obj map[string]any
	if json.Unmarshal([]byte(raw), &obj) != nil {
		return raw
	}
	out := map[string]any{}
	switch canonTool(name) {
	case "shell":
		if s, ok := argPresent(obj, "command", "cmd"); ok {
			out["command"] = s
		}
	case "read":
		if s, ok := argPresent(obj, "path", "file_path", "target_file"); ok {
			out["path"] = s
		}
	case "write":
		if s, ok := argPresent(obj, "path", "file_path"); ok {
			out["path"] = s
		}
		if s, ok := argPresent(obj, "content", "contents", "file_text", "stream_content"); ok {
			out["content"] = s
		}
	case "grep":
		if s, ok := argPresent(obj, "pattern", "query"); ok {
			out["pattern"] = s
		}
		if s, ok := argPresent(obj, "path", "file_path"); ok {
			out["path"] = s
		}
		if s, ok := argPresent(obj, "glob", "glob_pattern"); ok {
			out["glob"] = s
		}
	case "glob":
		if s, ok := argPresent(obj, "pattern", "glob_pattern", "glob", "query"); ok {
			out["pattern"] = s
		}
		if s, ok := argPresent(obj, "path", "target_directory"); ok {
			out["path"] = s
		}
	case "ls", "delete":
		if s, ok := argPresent(obj, "path", "file_path", "target_directory"); ok {
			out["path"] = s
		}
	case "edit":
		if editAsWrite(obj) {
			return displayArgs("Write", raw)
		}
		if s, ok := argPresent(obj, "path", "file_path"); ok {
			out["path"] = s
		}
		if s, ok := argPresent(obj, "old_string", "oldText", "old_str"); ok {
			out["old_string"] = s
		}
		if s, ok := argPresent(obj, "new_string", "newText", "new_str", "stream_content"); ok {
			out["new_string"] = s
		}
	default:
		return raw
	}
	if len(out) == 0 {
		return raw
	}
	b, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return string(b)
}

func argString(obj map[string]any, keys ...string) string {
	for _, k := range keys {
		v, ok := obj[k]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case string:
			return t
		default:
			b, err := json.Marshal(t)
			if err == nil {
				return string(b)
			}
		}
	}
	return ""
}

func protoValue(v any) []byte {
	var b wbuf
	switch t := v.(type) {
	case nil:
		b.u32(1, 0)
	case string:
		b.str(3, t)
	case bool:
		b.bool(4, t)
	case float64:
		b.fixed64(2, math.Float64bits(t))
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			b.str(3, t.String())
			break
		}
		b.fixed64(2, math.Float64bits(f))
	case map[string]any:
		var st wbuf
		for k, one := range t {
			var entry wbuf
			entry.str(1, k)
			entry.msg(2, protoValue(one))
			st.msg(1, []byte(entry))
		}
		b.msg(5, []byte(st))
	case []any:
		var list wbuf
		for _, one := range t {
			list.msg(1, protoValue(one))
		}
		b.msg(6, []byte(list))
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			b.u32(1, 0)
			break
		}
		b.str(3, string(raw))
	}
	return []byte(b)
}
