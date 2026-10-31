package cursoride

import (
	"encoding/json"
	"strings"
)

// replayTurns reads ConversationStateStructure.turns (field 8). Cursor's
// model prompt is root_prompt_messages_json; turns are the structured
// copy. They are used when that prompt is missing, which is a long
// session whose blob ids were not prefetched. A 32-byte value is a blob
// id. Anything else that parses is the turn itself.
func replayTurns(fs []field, prefetched map[string][]byte) (msgs []ChatMessage, missing int) {
	for _, raw := range fieldBytes(fs, 8) {
		body, miss := turnBytes(raw, prefetched)
		missing += miss
		if len(body) == 0 {
			continue
		}
		one, miss := messagesFromTurn(body, prefetched)
		missing += miss
		msgs = append(msgs, one...)
	}
	return msgs, missing
}

func turnBytes(raw []byte, prefetched map[string][]byte) ([]byte, int) {
	if looksJSON(raw) {
		return raw, 0
	}
	if len(raw) == 32 {
		body, miss := resolveBlob(raw, prefetched)
		if miss > 0 || len(body) == 0 {
			return nil, 1
		}
		return body, 0
	}
	if _, err := parseFields(raw); err != nil {
		return nil, 1
	}
	return raw, 0
}

func messagesFromTurn(body []byte, prefetched map[string][]byte) ([]ChatMessage, int) {
	if looksJSON(body) {
		msgs, err := messagesFromJSON(body)
		if err != nil {
			return nil, 1
		}
		return msgs, 0
	}
	fs, err := parseFields(body)
	if err != nil {
		return nil, 1
	}
	agents := fieldBytes(fs, 1)
	if len(agents) == 0 {
		return nil, 0
	}
	af, err := parseFields(agents[0])
	if err != nil {
		return nil, 1
	}
	var out []ChatMessage
	var missing int
	for _, raw := range fieldBytes(af, 1) {
		b, miss := turnBytes(raw, prefetched)
		missing += miss
		if len(b) == 0 {
			continue
		}
		if looksJSON(b) {
			msgs, err := messagesFromJSON(b)
			if err == nil {
				out = append(out, msgs...)
			}
			continue
		}
		uf, err := parseFields(b)
		if err != nil {
			continue
		}
		text, _ := fieldString(uf, 1)
		text = strings.TrimSpace(text)
		var attach userAttach
		if ctx := fieldBytes(uf, 3); len(ctx) > 0 {
			if cfs, err := parseFields(ctx[0]); err == nil {
				attach = attachFromContext(cfs, prefetched)
				missing += attach.missing
			}
		}
		if text == "" && attach.empty() {
			continue
		}
		out = append(out, ChatMessage{
			Role:    "user",
			Content: messageContent(joinNote(text, attach.noteFor(text)), attach.images),
		})
	}
	for _, raw := range fieldBytes(af, 2) {
		b, miss := turnBytes(raw, prefetched)
		missing += miss
		if len(b) == 0 {
			continue
		}
		out = append(out, messagesFromStep(b)...)
	}
	return out, missing
}

func messagesFromStep(body []byte) []ChatMessage {
	fs, err := parseFields(body)
	if err != nil {
		return nil
	}
	if asst := fieldBytes(fs, 1); len(asst) > 0 {
		af, err := parseFields(asst[0])
		if err == nil {
			if text, ok := fieldString(af, 1); ok && strings.TrimSpace(text) != "" {
				return []ChatMessage{{Role: "assistant", Content: strings.TrimSpace(text)}}
			}
		}
	}
	calls := fieldBytes(fs, 2)
	if len(calls) == 0 {
		return nil
	}
	return messagesFromToolStep(calls[0])
}

type turnArm struct {
	field int
	name  string
	pi    bool
}

// turnArms are ToolCall oneof fields that carry args (field 1) and a
// result (field 2). Pi fields match the exec arms this package sends.
var turnArms = []turnArm{
	{61, "Read", true},
	{62, "Shell", true},
	{63, "Edit", true},
	{64, "Write", true},
	{65, "Grep", true},
	{66, "Glob", true},
	{67, "LS", true},
	{8, "Read", false},
	{1, "Shell", false},
	{3, "Delete", false},
	{15, "MCP", false},
}

func messagesFromToolStep(body []byte) []ChatMessage {
	fs, err := parseFields(body)
	if err != nil {
		return nil
	}
	id, _ := fieldString(fs, 57)
	if id == "" {
		id = "call-turn"
	}
	for _, arm := range turnArms {
		raws := fieldBytes(fs, arm.field)
		if len(raws) == 0 {
			continue
		}
		args, result, images, name := decodeTurnArm(arm, raws[0])
		if name == "" {
			name = arm.name
		}
		out := []ChatMessage{{
			Role:      "assistant",
			ToolCalls: []ToolCall{{ID: id, Name: name, Arguments: args}},
		}}
		if result != "" || len(images) > 0 {
			out = append(out, ChatMessage{
				Role: "tool", ToolCallID: id, Content: messageContent(result, images),
			})
		}
		return out
	}
	return nil
}

func decodeTurnArm(arm turnArm, raw []byte) (args, result string, images []ContentPart, name string) {
	fs, err := parseFields(raw)
	if err != nil {
		return "{}", "", nil, ""
	}
	args = "{}"
	if arm.field == 15 {
		if a := fieldBytes(fs, 1); len(a) > 0 {
			args, name = mcpArgsJSON(a[0])
		}
	} else if a := fieldBytes(fs, 1); len(a) > 0 {
		args = turnArgsJSON(arm, a[0])
	}
	if r := fieldBytes(fs, 2); len(r) > 0 {
		kind := "pi-read"
		switch arm.name {
		case "Shell":
			kind = "shell"
			if arm.pi {
				kind = "pi-bash"
			}
		case "Edit":
			kind = "pi-edit"
		case "Delete":
			kind = "delete"
		case "MCP":
			kind = "mcp"
		case "Write":
			if !arm.pi {
				kind = "write"
			} else {
				kind = "pi-write"
			}
		case "Read":
			if !arm.pi {
				kind = "read"
			}
		}
		result, images = decodeExecBody(kind, r[0])
	}
	if args == "" {
		args = "{}"
	}
	return args, result, images, name
}

func turnArgsJSON(arm turnArm, raw []byte) string {
	fs, err := parseFields(raw)
	if err != nil {
		return "{}"
	}
	obj := map[string]any{}
	put := func(key string, n int) {
		if s, ok := fieldString(fs, n); ok && s != "" {
			obj[key] = s
		}
	}
	switch arm.name {
	case "Read":
		put("path", 1)
		if n, ok := fieldUint(fs, 2); ok {
			obj["offset"] = n
		}
		if n, ok := fieldUint(fs, 3); ok {
			obj["limit"] = n
		}
	case "Shell":
		put("command", 1)
	case "Write":
		put("path", 1)
		put("content", 2)
	case "Edit":
		put("path", 1)
		if reps := fieldBytes(fs, 2); len(reps) > 0 {
			rf, err := parseFields(reps[0])
			if err == nil {
				if s, ok := fieldString(rf, 1); ok {
					obj["old_string"] = s
				}
				if s, ok := fieldString(rf, 2); ok {
					obj["new_string"] = s
				}
			}
		}
	case "Grep":
		put("pattern", 1)
		put("path", 2)
		put("glob", 3)
	case "Glob":
		put("pattern", 1)
		put("path", 2)
	case "LS":
		put("path", 1)
	case "Delete":
		put("path", 1)
	}
	if len(obj) == 0 {
		return "{}"
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func mcpArgsJSON(raw []byte) (args, name string) {
	fs, err := parseFields(raw)
	if err != nil {
		return "{}", ""
	}
	name, _ = fieldString(fs, 5)
	if name == "" {
		name, _ = fieldString(fs, 1)
	}
	obj := map[string]any{}
	for _, ent := range fieldBytes(fs, 2) {
		ef, err := parseFields(ent)
		if err != nil {
			continue
		}
		k, _ := fieldString(ef, 1)
		v := fieldBytes(ef, 2)
		if k == "" || len(v) == 0 {
			continue
		}
		obj[k] = string(v[0])
	}
	if len(obj) == 0 {
		return "{}", name
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return "{}", name
	}
	return string(b), name
}
