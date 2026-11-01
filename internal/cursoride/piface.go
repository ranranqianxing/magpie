package cursoride

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// The Pi facade is the Agent composer's tool surface. Cursor 3.27 runs
// these calls as Pi*ExecArgs / Pi*ExecResult (cards 61–67, exec 45–51).
// One codec offers that surface to the model, encodes it onto the exec
// the client runs, and decodes the result the client sends back. Turn
// replay reads the same messages, so a later round sees the call the
// executor ran rather than a thinner copy of it.
//
// Field numbers are the modern agent protocol's, which matches
// cursor-agent-exec 3.27.22 for the args this package already checked
// (PiReadExecArgs, PiEditExecArgs). PiEditReplacement has old_text and
// new_text and no replace-all bool, so that flag is not offered and not
// encoded. A guess at its field number would be a different protocol.

func nativeAgentTools() []ToolSpec {
	return []ToolSpec{
		{Name: "Shell", Description: "Run a shell command in the workspace. timeout is an optional number of seconds.", Parameters: schema(`{"type":"object","properties":{"command":{"type":"string"},"timeout":{"type":"number"}},"required":["command"]}`)},
		{Name: "Read", Description: "Read a file. offset is the 1-indexed line to start at; limit is how many lines to return. Omit either to read from the start or through the end.", Parameters: schema(`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer","minimum":1},"limit":{"type":"integer","minimum":1}},"required":["path"]}`)},
		{Name: "Grep", Description: "Search files for a pattern. ignore_case and literal are executor flags. context is how many lines around a hit to return. limit caps matches.", Parameters: schema(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"glob":{"type":"string"},"ignore_case":{"type":"boolean"},"literal":{"type":"boolean"},"context":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1}},"required":["pattern"]}`)},
		{Name: "Delete", Description: "Delete a file.", Parameters: schema(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)},
		{Name: "Glob", Description: "Find files by a glob pattern. limit caps how many paths are returned.", Parameters: schema(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string"},"limit":{"type":"integer","minimum":1}},"required":["pattern"]}`)},
		{Name: "LS", Description: "List a directory. limit caps how many entries are returned.", Parameters: schema(`{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":"integer","minimum":1}}}`)},
		{Name: "Edit", Description: "Replace exact text in a file. Use old_string and new_string for one change, or edits for several, applied in order. Each old_string must match the file exactly. There is no replace-all flag; repeat a pair to change several sites.", Parameters: schema(`{"type":"object","properties":{"path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"},"edits":{"type":"array","items":{"type":"object","properties":{"old_string":{"type":"string"},"new_string":{"type":"string"}},"required":["old_string","new_string"]}}},"required":["path"]}`)},
		{Name: "Write", Description: "Write a file.", Parameters: schema(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`)},
	}
}

// encodeNativePi is the exec body for one composer tool. card and exec
// are the ToolCall and ExecServerMessage field numbers. ok is false for
// a tool this package does not canon; the caller then uses the MCP arm.
func encodeNativePi(name string, obj map[string]any) (card, exec int, args []byte, ok bool) {
	switch canonTool(name) {
	case "shell":
		return 62, 46, encodeBash(obj), true
	case "read":
		return 61, 45, encodeRead(obj), true
	case "grep":
		return 65, 49, encodeGrep(obj), true
	case "glob":
		return 66, 50, encodeFind(obj), true
	case "ls":
		return 67, 51, encodeLs(obj), true
	case "write":
		return 64, 48, encodeWrite(obj), true
	case "edit":
		if editAsWrite(obj) {
			return 64, 48, encodeWrite(obj), true
		}
		return 63, 47, encodeEdit(obj), true
	default:
		return 0, 0, nil, false
	}
}

func encodeBash(obj map[string]any) []byte {
	var b wbuf
	putArg(&b, 1, obj, "command", "cmd")
	putSeconds(&b, 2, obj, "timeout")
	return []byte(b)
}

// encodeRead is PiReadExecArgs. Cursor 3.27.22's cursor-agent-exec declares
// "PiReadExecArgs|1 path 9|2 offset 5?|3 limit 5?": path, then an optional
// int32 offset and limit. The executor treats offset as a 1-indexed line
// and limit as a line count (slice from offset-1, length limit).
func encodeRead(obj map[string]any) []byte {
	var b wbuf
	putArg(&b, 1, obj, "path", "file_path", "target_file")
	putI32(&b, 2, obj, "offset")
	putI32(&b, 3, obj, "limit")
	return []byte(b)
}

// encodeGrep is PiGrepExecArgs: pattern, path, glob, ignore_case, literal,
// context, limit. The last four are what a later edit uses to see the
// same lines the search showed.
func encodeGrep(obj map[string]any) []byte {
	var b wbuf
	putArg(&b, 1, obj, "pattern", "query")
	putArg(&b, 2, obj, "path", "file_path")
	putArg(&b, 3, obj, "glob", "glob_pattern")
	putBoolTrue(&b, 4, obj, "ignore_case", "case_insensitive")
	putBoolTrue(&b, 5, obj, "literal", "fixed_string")
	putI32(&b, 6, obj, "context", "context_lines")
	putI32(&b, 7, obj, "limit", "head_limit")
	return []byte(b)
}

func encodeFind(obj map[string]any) []byte {
	var b wbuf
	putArg(&b, 1, obj, "pattern", "glob_pattern", "glob", "query")
	putArg(&b, 2, obj, "path", "target_directory")
	putI32(&b, 3, obj, "limit")
	return []byte(b)
}

func encodeLs(obj map[string]any) []byte {
	var b wbuf
	putArg(&b, 1, obj, "path", "target_directory")
	putI32(&b, 2, obj, "limit")
	return []byte(b)
}

func encodeWrite(obj map[string]any) []byte {
	var b wbuf
	putArg(&b, 1, obj, "path", "file_path")
	putArg(&b, 2, obj, "content", "contents", "file_text", "stream_content")
	return []byte(b)
}

// encodeEdit is PiEditExecArgs: path (field 1) and repeated
// PiEditReplacement (field 2). Each replacement is old_text then new_text.
// A single old_string/new_string pair is one replacement. An edits array
// is one replacement per element, in order. replace_all is not a field.
func encodeEdit(obj map[string]any) []byte {
	var b wbuf
	putArg(&b, 1, obj, "path", "file_path")
	for _, pair := range editPairs(obj) {
		var rep wbuf
		rep.str(1, pair[0])
		rep.str(2, pair[1])
		b.msg(2, []byte(rep))
	}
	return []byte(b)
}

// editPairs is every replacement the exec will run. An edits array with
// at least one complete pair wins over a top-level pair, so a batch is
// not collapsed to its first hunk. A top-level pair is one replacement.
// new_string may be empty: that deletes the matched text.
func editPairs(obj map[string]any) [][2]string {
	if raw, ok := obj["edits"]; ok {
		if list, ok := raw.([]any); ok {
			var out [][2]string
			for _, item := range list {
				m, ok := item.(map[string]any)
				if !ok {
					continue
				}
				old, okOld := argPresent(m, "old_string", "oldText", "old_str", "old_text")
				newS, okNew := argPresent(m, "new_string", "newText", "new_str", "new_text")
				if !okOld || !okNew || strings.TrimSpace(old) == "" {
					continue
				}
				out = append(out, [2]string{old, newS})
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	old, okOld := argPresent(obj, "old_string", "oldText", "old_str")
	newS, okNew := argPresent(obj, "new_string", "newText", "new_str", "stream_content")
	if !okOld || !okNew || strings.TrimSpace(old) == "" {
		return nil
	}
	return [][2]string{{old, newS}}
}

func putBoolTrue(b *wbuf, field int, obj map[string]any, keys ...string) {
	v, ok := argBool(obj, keys...)
	if ok && v {
		b.bool(field, true)
	}
}

func argBool(obj map[string]any, keys ...string) (bool, bool) {
	for _, k := range keys {
		v, ok := obj[k]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case bool:
			return t, true
		case string:
			switch strings.ToLower(strings.TrimSpace(t)) {
			case "true":
				return true, true
			case "false":
				return false, true
			}
		}
	}
	return false, false
}

func putSeconds(b *wbuf, field int, obj map[string]any, keys ...string) {
	n, ok := argSeconds(obj, keys...)
	if !ok {
		return
	}
	b.fixed64(field, math.Float64bits(n))
}

func argSeconds(obj map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		v, ok := obj[k]
		if !ok || v == nil {
			continue
		}
		var f float64
		switch n := v.(type) {
		case float64:
			f = n
		case int:
			f = float64(n)
		case json.Number:
			x, err := n.Float64()
			if err != nil {
				return 0, false
			}
			f = x
		default:
			continue
		}
		if f <= 0 || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

func copyPiGrepFlags(obj, out map[string]any) {
	if v, ok := argBool(obj, "ignore_case", "case_insensitive"); ok && v {
		out["ignore_case"] = true
	}
	if v, ok := argBool(obj, "literal", "fixed_string"); ok && v {
		out["literal"] = true
	}
	if n, ok := argI32(obj, "context", "context_lines"); ok {
		out["context"] = n
	}
	if n, ok := argI32(obj, "limit", "head_limit"); ok {
		out["limit"] = n
	}
}

// piSkipReason is the tool result for a sealed call the executor was not
// given. Cursor's own exec always answers with a result oneof. Dropping
// the call ends the turn, and the model retries the same empty arguments.
// The text names the missing field, not the argument value.
func piSkipReason(name, args string) string {
	label := strings.TrimSpace(name)
	if label == "" {
		label = "tool"
	}
	args = strings.TrimSpace(args)
	if !json.Valid([]byte(args)) {
		return label + " was not run: arguments are not json"
	}
	var obj map[string]any
	if json.Unmarshal([]byte(args), &obj) != nil {
		return label + " was not run: arguments are not json"
	}
	switch canonTool(name) {
	case "shell":
		return label + " was not run: missing command"
	case "read", "delete":
		return label + " was not run: missing path"
	case "grep", "glob":
		return label + " was not run: missing pattern"
	case "write":
		if !argNonBlank(obj, "path", "file_path") {
			return label + " was not run: missing path"
		}
		return label + " was not run: missing content"
	case "edit":
		if editAsWrite(obj) {
			return piSkipReason("Write", args)
		}
		if !argNonBlank(obj, "path", "file_path") {
			return label + " was not run: missing path"
		}
		return label + " was not run: missing old_string and new_string"
	default:
		return label + " was not run: arguments are empty"
	}
}

// fillPiTurnArgs copies Pi exec args that the generic string walk does
// not. One edit stays old_string/new_string. Several edits stay an edits
// array, so a later round does not replay only the first hunk.
func fillPiTurnArgs(name string, fs []field, obj map[string]any) {
	switch name {
	case "Shell":
		if n, ok := fieldFloat(fs, 2); ok && n > 0 {
			obj["timeout"] = n
		}
	case "Edit":
		reps := fieldBytes(fs, 2)
		if len(reps) == 0 {
			return
		}
		if len(reps) == 1 {
			old, newS := editPairFields(reps[0])
			if old != "" {
				obj["old_string"] = old
			}
			if newS != "" || old != "" {
				obj["new_string"] = newS
			}
			return
		}
		var edits []map[string]string
		for _, raw := range reps {
			old, newS := editPairFields(raw)
			if old == "" {
				continue
			}
			edits = append(edits, map[string]string{"old_string": old, "new_string": newS})
		}
		if len(edits) > 0 {
			delete(obj, "old_string")
			delete(obj, "new_string")
			obj["edits"] = edits
		}
	case "Grep":
		if n, ok := fieldUint(fs, 4); ok && n != 0 {
			obj["ignore_case"] = true
		}
		if n, ok := fieldUint(fs, 5); ok && n != 0 {
			obj["literal"] = true
		}
		if n, ok := fieldUint(fs, 6); ok {
			obj["context"] = n
		}
		if n, ok := fieldUint(fs, 7); ok {
			obj["limit"] = n
		}
	case "Glob":
		if n, ok := fieldUint(fs, 3); ok {
			obj["limit"] = n
		}
	case "LS":
		if n, ok := fieldUint(fs, 2); ok {
			obj["limit"] = n
		}
	}
}

func editPairFields(raw []byte) (old, newS string) {
	rf, err := parseFields(raw)
	if err != nil {
		return "", ""
	}
	old, _ = fieldString(rf, 1)
	newS, _ = fieldString(rf, 2)
	return old, newS
}

func fieldFloat(fs []field, num int) (float64, bool) {
	for _, f := range fs {
		if f.num == num && f.wt == 1 && len(f.b) == 8 {
			return math.Float64frombits(binary.LittleEndian.Uint64(f.b)), true
		}
	}
	return 0, false
}

// readExecOutcome is one finished exec arm. status is ok, error,
// rejected, or empty. An empty Pi oneof is not a success: the executor
// treats an unset result as "the tool ran and produced nothing", and the
// model then edits as if the previous call worked. truncated is true when
// the success or the bash error carries PiTruncation, or a search hit its
// limit. The note is part of the tool text the model sees. It is not logged.
type execOutcome struct {
	text      string
	images    []ContentPart
	status    string
	truncated bool
}

func readExecOutcome(kind string, body []byte) execOutcome {
	fs, err := parseFields(body)
	if err != nil {
		if isPiKind(kind) {
			return execOutcome{status: "empty", text: "error: tool result was empty"}
		}
		return execOutcome{status: "empty"}
	}
	if raws := fieldBytes(fs, 1); len(raws) > 0 {
		if isPiKind(kind) {
			text, trunc := piSuccessText(kind, raws[0])
			return execOutcome{text: text, status: "ok", truncated: trunc}
		}
		text, images := successText(kind, raws[0])
		return execOutcome{text: text, images: images, status: "ok"}
	}
	if raws := fieldBytes(fs, 2); len(raws) > 0 {
		text := failText("error: ", raws[0])
		trunc := false
		if isPiKind(kind) {
			text, trunc = piFailureText(kind, raws[0], text)
		}
		return execOutcome{text: text, status: "error", truncated: trunc}
	}
	if raws := fieldBytes(fs, 3); len(raws) > 0 {
		return execOutcome{text: failText("rejected: ", raws[0]), status: "rejected"}
	}
	if isPiKind(kind) {
		return execOutcome{status: "empty", text: "error: tool result was empty"}
	}
	return execOutcome{status: "empty"}
}

func isPiKind(kind string) bool {
	switch kind {
	case "pi-read", "pi-bash", "pi-edit", "pi-write", "pi-grep", "pi-find", "pi-ls":
		return true
	default:
		return false
	}
}

func piSuccessText(kind string, msg []byte) (string, bool) {
	fs, err := parseFields(msg)
	if err != nil {
		return "", false
	}
	var text string
	if kind == "pi-edit" {
		text = piEditText(fs)
	} else {
		text, _ = fieldString(fs, 1)
		text = decoratePiOutput(kind, fs, text)
	}
	note := truncationNote(kind, fs)
	if extra := limitNote(kind, fs); extra != "" {
		note = joinNote(note, extra)
	}
	return joinNote(text, note), note != ""
}

func piFailureText(kind string, msg []byte, text string) (string, bool) {
	fs, err := parseFields(msg)
	if err != nil {
		return text, false
	}
	if strings.TrimSpace(text) == "" || text == "error" {
		text = "error: tool failed"
	}
	note := truncationNote(kind, fs)
	if kind == "pi-bash" {
		if p, ok := fieldString(fs, 3); ok && strings.TrimSpace(p) != "" {
			note = joinNote(note, "full output: "+strings.TrimSpace(p))
		}
	}
	return joinNote(text, note), note != "" && strings.Contains(note, "[truncated")
}

func piEditText(fs []field) string {
	out, _ := fieldString(fs, 1)
	diff, _ := fieldString(fs, 2)
	patch, _ := fieldString(fs, 3)
	out = strings.TrimSpace(out)
	diff = strings.TrimSpace(diff)
	patch = strings.TrimSpace(patch)
	text := out
	if diff != "" && (text == "" || !strings.Contains(text, diff)) {
		text = joinNote(text, diff)
	}
	if patch != "" && (text == "" || !strings.Contains(text, patch)) {
		text = joinNote(text, patch)
	}
	if n, ok := fieldUint(fs, 4); ok && n != 0 {
		text = joinNote(text, fmt.Sprintf("first_changed_line=%d", n))
	}
	return text
}

func decoratePiOutput(kind string, fs []field, text string) string {
	if kind == "pi-bash" {
		if p, ok := fieldString(fs, 3); ok && strings.TrimSpace(p) != "" {
			text = joinNote(text, "full output: "+strings.TrimSpace(p))
		}
	}
	return text
}

func limitNote(kind string, fs []field) string {
	switch kind {
	case "pi-grep", "pi-find", "pi-ls":
		var parts []string
		if n, ok := fieldUint(fs, 3); ok && n != 0 {
			parts = append(parts, "limit reached")
		}
		if n, ok := fieldUint(fs, 4); ok && n != 0 {
			parts = append(parts, "lines truncated")
		}
		return strings.Join(parts, ", ")
	default:
		return ""
	}
}

// truncationNote reads PiTruncation when this message's field 2 is that
// struct. PiRead, PiBash, PiGrep, PiFind and PiLs successes use it there,
// and so does PiBashExecError. PiEdit's field 2 is the diff string, so it
// is not parsed as a truncation: a diff can look like a protobuf.
func truncationNote(kind string, fs []field) string {
	switch kind {
	case "pi-read", "pi-bash", "pi-grep", "pi-find", "pi-ls":
	default:
		return ""
	}
	raws := fieldBytes(fs, 2)
	if len(raws) == 0 {
		return ""
	}
	tf, err := parseFields(raws[0])
	if err != nil {
		return ""
	}
	on, ok := fieldUint(tf, 1)
	if !ok || on == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[truncated")
	if by, ok := fieldString(tf, 2); ok {
		by = oneToken(by)
		if by != "" {
			b.WriteString(" by=")
			b.WriteString(by)
		}
	}
	if n, ok := fieldUint(tf, 3); ok {
		fmt.Fprintf(&b, " total_lines=%d", n)
	}
	if n, ok := fieldUint(tf, 4); ok {
		fmt.Fprintf(&b, " output_lines=%d", n)
	}
	b.WriteByte(']')
	return b.String()
}

func oneToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, " \n\r\t") {
		return ""
	}
	if len(s) > 24 {
		s = s[:24]
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return ""
		}
	}
	return s
}

const truncatedStepNote = "A previous tool step was truncated and is not in this prompt."
