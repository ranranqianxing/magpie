package cursoride

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// DeepSeek V4 writes a tool call as DSML in the reply text. A relay that
// also emits a native tool_call often leaves its arguments as {}. The
// path the UI shows is in that text; the exec has to use it. The same
// reply may use a <tool_call> JSON block instead. Free prose is not read:
// a sentence that names a path is not a call.

var (
	dsmlTag    = `<\s*/?\s*[|｜][\s|｜]*DSML[\s|｜]*`
	dsmlInvoke = regexp.MustCompile(`<\s*[|｜][\s|｜]*DSML[\s|｜]*invoke\s+name\s*=\s*"([^"]+)"\s*>`)
	dsmlParam  = regexp.MustCompile(`(?s)<\s*[|｜][\s|｜]*DSML[\s|｜]*parameter\s+name\s*=\s*"([^"]+)"((?:\s+\w+\s*=\s*"[^"]*")*)\s*>(.*?)` + dsmlTag + `parameter\s*>`)
	dsmlString = regexp.MustCompile(`string\s*=\s*"true"`)
)

type writtenArgs struct {
	name string
	args string
}

// fillEmptyToolArgs replaces a sealed call whose arguments are empty or
// "{}" with parameters of the same tool written in text. A call that
// already has fields is left as it is. One line is logged when a call is
// still empty, or when markup supplied the arguments, so a later run can
// tell a placeholder from a call the model left empty. Values are not logged.
func fillEmptyToolArgs(calls []toolBuf, text string) {
	found := markupCalls(text)
	used := make([]bool, len(found))
	for i := range calls {
		before := calls[i].args
		if !callArgsEmpty(before) {
			continue
		}
		for j, f := range found {
			if used[j] || !sameTool(f.name, calls[i].name) {
				continue
			}
			calls[i].args = f.args
			used[j] = true
			break
		}
		noteSealedArgs(calls[i].name, before, calls[i].args)
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

func noteSealedArgs(name, before, after string) {
	if !callArgsEmpty(before) {
		return
	}
	if !callArgsEmpty(after) {
		Note(fmt.Sprintf("agent tool args name=%s from=markup bytes=%d", name, len(after)))
		return
	}
	Note(fmt.Sprintf("agent tool seal name=%s args=%d", name, len(strings.TrimSpace(after))))
}

func markupCalls(text string) []writtenArgs {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var out []writtenArgs
	out = append(out, dsmlCalls(text)...)
	out = append(out, toolTagCalls(text)...)
	return out
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
		out = append(out, writtenArgs{name: name, args: b})
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
		out = append(out, writtenArgs{name: v.Name, args: args})
	}
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
