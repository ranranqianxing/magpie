package cursoride

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// DeepSeek V4 writes a tool call in the reply text, and a relay that also
// emits a native tool_call often leaves its arguments as {}. The gateway
// does not parse that text once a native call exists, so the parameters
// reach this package in the assistant text. The forms read here are the
// ones the gateway already accepts when no native call is present: a DSML
// invoke, a <tool_call> JSON object, DeepSeek's {"name":…} followed by
// <parameter> tags (#917), and GLM's <arg_key>/<arg_value> pairs. Free
// prose is not read: a sentence that names a path is not a call.

var (
	dsmlTag    = `<\s*/?\s*[|｜][\s|｜]*DSML[\s|｜]*`
	dsmlInvoke = regexp.MustCompile(`<\s*[|｜][\s|｜]*DSML[\s|｜]*invoke\s+name\s*=\s*"([^"]+)"\s*>`)
	dsmlParam  = regexp.MustCompile(`(?s)<\s*[|｜][\s|｜]*DSML[\s|｜]*parameter\s+name\s*=\s*"([^"]+)"((?:\s+\w+\s*=\s*"[^"]*")*)\s*>(.*?)` + dsmlTag + `parameter\s*>`)
	dsmlString = regexp.MustCompile(`string\s*=\s*"true"`)
	paramHead  = regexp.MustCompile(`^\s*\{\s*"name"\s*:\s*"([^"]+)"\s*\}?\s*>?`)
	paramOpen  = regexp.MustCompile(`^\s*<parameter(?:\s+name\s*=\s*"([^"]+)"|=([^\s>]+))\s*>`)
	glmPair    = regexp.MustCompile(`(?s)^\s*<arg_key>(.*?)</arg_key>\s*<arg_value>(.*?)</arg_value>`)
	glmName    = regexp.MustCompile(`^\s*([^\s<>{}"]+)\s*`)
)

type writtenArgs struct {
	name string
	args string
	from string
}

// fillEmptyToolArgs replaces a sealed call whose arguments are empty or
// "{}" with parameters of the same tool written in text. A call that
// already has fields is left as it is. fill records which form supplied
// the parameters. Values are not logged here; the round logs one trace.
func fillEmptyToolArgs(calls []toolBuf, text string) {
	found := markupCalls(text)
	used := make([]bool, len(found))
	for i := range calls {
		if !callArgsEmpty(calls[i].args) {
			continue
		}
		for j, f := range found {
			if used[j] || !sameTool(f.name, calls[i].name) || callArgsEmpty(f.args) {
				continue
			}
			calls[i].args = f.args
			calls[i].fill = f.from
			used[j] = true
			break
		}
	}
}

func sameTool(a, b string) bool {
	if strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b)) {
		return true
	}
	ca, cb := canonTool(a), canonTool(b)
	return ca != "" && ca == cb
}

func callArgsEmpty(s string) bool {
	s = strings.TrimSpace(s)
	return s == "" || emptyJSONObject(s)
}

func markupCalls(text string) []writtenArgs {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var out []writtenArgs
	out = append(out, dsmlCalls(text)...)
	out = append(out, toolTagCalls(text)...)
	out = append(out, blockCalls(text)...)
	return out
}

// markupMarkers names which call shapes the reply text contains. It is
// the diagnostic when a fill does not match: the next run can see that
// the path sat in a parameter tag, or in none of these shapes. The text
// itself is not returned.
func markupMarkers(text string) string {
	var marks []string
	if strings.Contains(text, "DSML") {
		marks = append(marks, "dsml")
	}
	if strings.Contains(text, "<tool_call>") {
		marks = append(marks, "tool_call")
	}
	if strings.Contains(text, "<parameter") {
		marks = append(marks, "parameter")
	}
	if strings.Contains(text, "<arg_key>") {
		marks = append(marks, "arg_key")
	}
	if len(marks) == 0 {
		return "none"
	}
	return strings.Join(marks, ",")
}

// noteToolTrace is one line for a sealed call or a gateway request whose
// assistant tool_calls were empty. Key names are logged. Values are not.
func noteToolTrace(where string, name, args string, frags, placeholders int, fill, markers, exec string) {
	if fill == "" {
		fill = "none"
	}
	if markers == "" {
		markers = "-"
	}
	if exec == "" {
		exec = "-"
	}
	Note(fmt.Sprintf(
		"agent tool trace where=%s name=%s frags=%d placeholder=%d empty=%t keys=%s fill=%s markers=%s exec=%s",
		where, safeToolName(name), frags, placeholders, callArgsEmpty(args), argKeyList(args), fill, markers, exec,
	))
}

func safeToolName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "?"
	}
	if len(name) > 40 {
		name = name[:40]
	}
	for _, r := range name {
		if r < 0x21 || r > 0x7e {
			return "?"
		}
	}
	return name
}

func argKeyList(args string) string {
	if callArgsEmpty(args) {
		return "-"
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &obj) != nil {
		return "?"
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		if !safeKey(k) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 8 {
		keys = keys[:8]
	}
	if len(keys) == 0 {
		return "-"
	}
	return strings.Join(keys, ",")
}

func safeKey(k string) bool {
	if k == "" || len(k) > 24 {
		return false
	}
	for _, r := range k {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

func dsmlCalls(text string) []writtenArgs {
	locs := dsmlInvoke.FindAllStringSubmatchIndex(text, -1)
	var out []writtenArgs
	for i, loc := range locs {
		if len(loc) < 4 {
			continue
		}
		name := text[loc[2]:loc[3]]
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		args := map[string]any{}
		for _, p := range dsmlParam.FindAllStringSubmatch(text[loc[1]:end], -1) {
			if len(p) < 4 || p[1] == "" {
				continue
			}
			val := p[3]
			if dsmlString.MatchString(p[2]) {
				args[p[1]] = val
				continue
			}
			var v any
			if json.Unmarshal([]byte(strings.TrimSpace(val)), &v) == nil {
				args[p[1]] = v
			} else {
				args[p[1]] = val
			}
		}
		if len(args) == 0 {
			continue
		}
		b, err := marshalArgs(args)
		if err != nil || callArgsEmpty(b) {
			continue
		}
		out = append(out, writtenArgs{name: name, args: b, from: "dsml"})
	}
	return out
}

func toolTagCalls(text string) []writtenArgs {
	const open = "<tool_call>"
	var out []writtenArgs
	rest := text
	for {
		i := strings.Index(rest, open)
		if i < 0 {
			return out
		}
		rest = strings.TrimLeft(rest[i+len(open):], " \t\r\n")
		if !strings.HasPrefix(rest, "{") {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(rest))
		var v struct {
			Name       string          `json:"name"`
			Arguments  json.RawMessage `json:"arguments"`
			Parameters json.RawMessage `json:"parameters"`
			Input      json.RawMessage `json:"input"`
		}
		if dec.Decode(&v) != nil || v.Name == "" {
			continue
		}
		raw := v.Arguments
		if callRawEmpty(raw) {
			raw = v.Parameters
		}
		if callRawEmpty(raw) {
			raw = v.Input
		}
		args, ok := normalizeArgRaw(raw)
		if !ok {
			continue
		}
		out = append(out, writtenArgs{name: v.Name, args: args, from: "tool_call"})
	}
}

// blockCalls reads <tool_call> blocks whose parameters are not a JSON
// arguments object: {"name":"Read"> then <parameter name="path">, or
// GLM's <arg_key>/<arg_value> pairs. A JSON object that already has
// arguments is left to toolTagCalls.
func blockCalls(text string) []writtenArgs {
	var out []writtenArgs
	rest := text
	for {
		i := strings.Index(rest, "<tool_call>")
		if i < 0 {
			return out
		}
		body := rest[i+len("<tool_call>"):]
		rest = body
		if c, n, ok := parameterCall(body); ok && n > 0 {
			out = append(out, c)
			rest = body[n:]
			continue
		}
		if c, n, ok := glmBlock(body); ok && n > 0 {
			out = append(out, c)
			rest = body[n:]
		}
	}
}

func parameterCall(s string) (writtenArgs, int, bool) {
	lead := len(s) - len(strings.TrimLeft(s, " \t\r\n"))
	body := s[lead:]
	name, n := "", 0
	if strings.HasPrefix(body, "{") {
		dec := json.NewDecoder(strings.NewReader(body))
		var v struct {
			Name string `json:"name"`
		}
		if dec.Decode(&v) == nil && v.Name != "" {
			name = v.Name
			n = int(dec.InputOffset())
			for n < len(body) && (body[n] == ' ' || body[n] == '\t' || body[n] == '\n' || body[n] == '\r') {
				n++
			}
			if n < len(body) && body[n] == '>' {
				n++
			}
		}
	}
	if name == "" {
		m := paramHead.FindStringSubmatchIndex(body)
		if m == nil {
			return writtenArgs{}, 0, false
		}
		name = body[m[2]:m[3]]
		n = m[1]
	}
	args := map[string]any{}
	for {
		p := paramOpen.FindStringSubmatchIndex(body[n:])
		if p == nil {
			break
		}
		var key string
		if p[2] >= 0 {
			key = body[n+p[2] : n+p[3]]
		} else {
			key = body[n+p[4] : n+p[5]]
		}
		if !safeKey(key) {
			break
		}
		n += p[1]
		end, next := len(body), len(body)
		for _, stop := range []string{"</parameter>", "<parameter", "</tool_call>", "<tool_call>"} {
			if i := strings.Index(body[n:], stop); i >= 0 && n+i < end {
				end, next = n+i, n+i
				if stop == "</parameter>" {
					next += len(stop)
				}
			}
		}
		val := strings.Trim(body[n:end], "\r\n")
		var v any
		if json.Unmarshal([]byte(strings.TrimSpace(val)), &v) == nil {
			args[key] = v
		} else {
			args[key] = val
		}
		n = next
	}
	if len(args) == 0 {
		return writtenArgs{}, 0, false
	}
	if t := strings.TrimLeft(body[n:], " \t\r\n"); strings.HasPrefix(t, "</tool_call>") {
		n = len(body) - len(t) + len("</tool_call>")
	}
	b, err := marshalArgs(args)
	if err != nil || callArgsEmpty(b) {
		return writtenArgs{}, 0, false
	}
	return writtenArgs{name: name, args: b, from: "parameter"}, lead + n, true
}

func glmBlock(s string) (writtenArgs, int, bool) {
	trimmed := strings.TrimLeft(s, " \t\r\n")
	if trimmed == "" || trimmed[0] == '{' || trimmed[0] == '<' {
		return writtenArgs{}, 0, false
	}
	m := glmName.FindStringSubmatchIndex(s)
	if m == nil {
		return writtenArgs{}, 0, false
	}
	name, n := s[m[2]:m[3]], m[1]
	if !safeToolNameOK(name) {
		return writtenArgs{}, 0, false
	}
	args := map[string]any{}
	for {
		p := glmPair.FindStringSubmatchIndex(s[n:])
		if p == nil {
			break
		}
		key := strings.TrimSpace(s[n+p[2] : n+p[3]])
		if !safeKey(key) {
			break
		}
		val := s[n+p[4] : n+p[5]]
		var v any
		if json.Unmarshal([]byte(strings.TrimSpace(val)), &v) == nil {
			args[key] = v
		} else {
			args[key] = val
		}
		n += p[1]
	}
	if len(args) == 0 {
		return writtenArgs{}, 0, false
	}
	if t := strings.TrimLeft(s[n:], " \t\r\n"); strings.HasPrefix(t, "</tool_call>") {
		n = len(s) - len(t) + len("</tool_call>")
	}
	b, err := marshalArgs(args)
	if err != nil || callArgsEmpty(b) {
		return writtenArgs{}, 0, false
	}
	return writtenArgs{name: name, args: b, from: "glm"}, n, true
}

func safeToolNameOK(name string) bool {
	return safeToolName(name) == name && name != "?"
}

func marshalArgs(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}

func callRawEmpty(raw json.RawMessage) bool {
	s, ok := normalizeArgRaw(raw)
	return !ok || callArgsEmpty(s)
}

func normalizeArgRaw(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", false
		}
		s = strings.TrimSpace(s)
		if !json.Valid([]byte(s)) || !strings.HasPrefix(s, "{") {
			return "", false
		}
		return s, true
	}
	if !json.Valid(raw) || raw[0] != '{' {
		return "", false
	}
	return string(raw), true
}
