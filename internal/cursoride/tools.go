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
		t.args += d.Args
	}
	if t.id == "" {
		t.id = "call-" + itoa(d.Index+1)
	}
	return t.id, t.name, t.take()
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
		{Name: "Grep", Description: "Search files for a pattern.", Parameters: schema(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"}},"required":["pattern"]}`)},
		{Name: "Delete", Description: "Delete a file.", Parameters: schema(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)},
		{Name: "LS", Description: "List a directory.", Parameters: schema(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
		{Name: "Edit", Description: "Replace the contents of a file.", Parameters: schema(`{"type":"object","properties":{"path":{"type":"string"},"stream_content":{"type":"string"}},"required":["path"]}`)},
		{Name: "Write", Description: "Write a file.", Parameters: schema(`{"type":"object","properties":{"path":{"type":"string"},"file_text":{"type":"string"}},"required":["path","file_text"]}`)},
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
// new tail, not the whole arguments string.
func agentPartialFrame(id, name, args string) []byte {
	var part wbuf
	part.str(1, id)
	if call := agentToolCall(id, name, args, false); len(call) > 0 {
		part.msg(2, call)
	}
	part.str(3, args)
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
// JSON object; a partial frame still names the tool so the card can open.
func toolBody(id, name, args string, full bool) (int, []byte) {
	obj := map[string]any{}
	if full {
		_ = json.Unmarshal([]byte(args), &obj)
	}
	switch canonTool(name) {
	case "shell":
		var shell wbuf
		shell.str(1, argString(obj, "command", "cmd"))
		shell.str(4, id)
		var call wbuf
		call.msg(1, []byte(shell))
		return 1, []byte(call)
	case "read":
		var read wbuf
		read.str(1, argString(obj, "path", "file_path", "target_file"))
		var call wbuf
		call.msg(1, []byte(read))
		return 8, []byte(call)
	case "grep":
		var grep wbuf
		grep.str(1, argString(obj, "pattern", "query"))
		if p := argString(obj, "path"); p != "" {
			grep.str(2, p)
		}
		var call wbuf
		call.msg(1, []byte(grep))
		return 5, []byte(call)
	case "delete":
		var del wbuf
		del.str(1, argString(obj, "path", "file_path"))
		var call wbuf
		call.msg(1, []byte(del))
		return 3, []byte(call)
	case "ls":
		var ls wbuf
		ls.str(1, argString(obj, "path", "target_directory"))
		var call wbuf
		call.msg(1, []byte(ls))
		return 13, []byte(call)
	case "edit":
		var edit wbuf
		edit.str(1, argString(obj, "path", "file_path"))
		if s := argString(obj, "stream_content", "new_string", "file_text", "contents"); s != "" {
			edit.str(6, s)
		}
		var call wbuf
		call.msg(1, []byte(edit))
		return 12, []byte(call)
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
	switch canonTool(name) {
	case "shell":
		var shell wbuf
		shell.str(1, argString(obj, "command", "cmd"))
		shell.str(4, id)
		exec.msg(2, []byte(shell))
	case "write":
		var w wbuf
		w.str(1, argString(obj, "path", "file_path"))
		w.str(2, argString(obj, "file_text", "contents", "content", "stream_content"))
		w.str(3, id)
		exec.msg(3, []byte(w))
	case "delete":
		var d wbuf
		d.str(1, argString(obj, "path", "file_path"))
		d.str(2, id)
		exec.msg(4, []byte(d))
	case "grep":
		var g wbuf
		g.str(1, argString(obj, "pattern", "query"))
		if p := argString(obj, "path"); p != "" {
			g.str(2, p)
		}
		g.str(14, id)
		exec.msg(5, []byte(g))
	case "read":
		var r wbuf
		r.str(1, argString(obj, "path", "file_path", "target_file"))
		r.str(2, id)
		exec.msg(7, []byte(r))
	case "ls":
		var ls wbuf
		ls.str(1, argString(obj, "path", "target_directory"))
		ls.str(3, id)
		exec.msg(8, []byte(ls))
	case "edit":
		var w wbuf
		w.str(1, argString(obj, "path", "file_path"))
		w.str(2, argString(obj, "stream_content", "new_string", "file_text", "contents"))
		w.str(3, id)
		exec.msg(3, []byte(w))
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
	case "edit", "strreplace", "str_replace", "search_replace":
		return "edit"
	case "write", "write_file":
		return "write"
	default:
		return ""
	}
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
