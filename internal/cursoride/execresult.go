package cursoride

import (
	"fmt"
	"strings"
)

// execToolResults reads one AgentClientMessage for tool results Cursor's
// executor writes after an exec. The JSON history shape is not what that
// executor sends: the follow-up is ExecClientMessage (field 2), and a
// failure that never ran is ExecClientControlMessage.throw (field 5,
// inner field 2).
//
// ExecClientMessage.id (field 1) repeats ExecServerMessage.id. exec_id
// (field 15) repeats the tool-call id this package put on the exec.
// Pi results are fields 46–52 (read, bash, edit, write, grep, find, ls).
// A shell stream (field 14), a heartbeat, and a stream close are not a
// finished result: the caller keeps waiting.
//
// execOf maps that uint32 id to the tool-call id. A throw carries only
// the uint32. want is the call ids still open. Text is not logged.
func execToolResults(payload []byte, want map[string]bool, execOf map[uint32]string) []ChatMessage {
	if len(want) == 0 || len(payload) == 0 {
		return nil
	}
	fs, err := parseFields(payload)
	if err != nil {
		return nil
	}
	var out []ChatMessage
	for _, raw := range fieldBytes(fs, 2) {
		if m, ok := oneExecResult(raw, want, execOf); ok {
			out = append(out, m)
		}
	}
	for _, raw := range fieldBytes(fs, 5) {
		if m, ok := oneExecThrow(raw, want, execOf); ok {
			out = append(out, m)
		}
	}
	return out
}

// toolFrameQuiet is a follow-up that is not a finished tool result and
// is not worth a diagnostic line: a client heartbeat, an exec heartbeat,
// or one chunk of a shell stream.
func toolFrameQuiet(payload []byte) bool {
	fs, err := parseFields(payload)
	if err != nil {
		return false
	}
	if hasField(fs, 7) && !hasField(fs, 2) && !hasField(fs, 5) {
		return true
	}
	for _, raw := range fieldBytes(fs, 2) {
		ef, err := parseFields(raw)
		if err != nil {
			continue
		}
		if hasField(ef, 14) {
			return true
		}
	}
	for _, raw := range fieldBytes(fs, 5) {
		cf, err := parseFields(raw)
		if err != nil {
			continue
		}
		if hasField(cf, 3) && !hasField(cf, 2) {
			return true
		}
	}
	return false
}

func noteUnparsedToolFrame(payload []byte) {
	if toolFrameQuiet(payload) {
		return
	}
	fs, err := parseFields(payload)
	if err != nil {
		Note("agent tool frame unrecognized")
		return
	}
	nums := make([]string, 0, len(fs))
	seen := map[int]bool{}
	for _, f := range fs {
		if seen[f.num] {
			continue
		}
		seen[f.num] = true
		nums = append(nums, itoa(f.num))
	}
	if len(nums) == 0 {
		Note("agent tool frame unrecognized")
		return
	}
	Note("agent tool frame fields=" + strings.Join(nums, ","))
}

func oneExecResult(raw []byte, want map[string]bool, execOf map[uint32]string) (ChatMessage, bool) {
	fs, err := parseFields(raw)
	if err != nil {
		return ChatMessage{}, false
	}
	execID, _ := fieldString(fs, 15)
	n, _ := fieldUint(fs, 1)
	kind, body, ok := execResultBody(fs)
	if !ok {
		return ChatMessage{}, false
	}
	text, images := decodeExecBody(kind, body)
	call, matched := callForExec(execID, uint32(n), want, execOf)
	Note(fmt.Sprintf("agent exec result kind=%s matched=%t bytes=%d images=%d", kind, matched, len(text), len(images)))
	if !matched {
		return ChatMessage{}, false
	}
	return ChatMessage{Role: "tool", ToolCallID: call, Content: messageContent(text, images)}, true
}

func oneExecThrow(raw []byte, want map[string]bool, execOf map[uint32]string) (ChatMessage, bool) {
	fs, err := parseFields(raw)
	if err != nil {
		return ChatMessage{}, false
	}
	throws := fieldBytes(fs, 2)
	if len(throws) == 0 {
		return ChatMessage{}, false
	}
	tf, err := parseFields(throws[0])
	if err != nil {
		return ChatMessage{}, false
	}
	n, _ := fieldUint(tf, 1)
	call, matched := callForExec("", uint32(n), want, execOf)
	errText, _ := fieldString(tf, 2)
	errText = strings.TrimSpace(errText)
	if errText == "" {
		errText = "error"
	} else {
		errText = "error: " + errText
	}
	Note(fmt.Sprintf("agent exec result kind=throw matched=%t bytes=%d images=0", matched, len(errText)))
	if !matched {
		return ChatMessage{}, false
	}
	return ChatMessage{Role: "tool", ToolCallID: call, Content: errText}, true
}

func callForExec(execID string, n uint32, want map[string]bool, execOf map[uint32]string) (string, bool) {
	if execID != "" && want[execID] {
		return execID, true
	}
	if execOf != nil {
		if id := execOf[n]; id != "" && want[id] {
			return id, true
		}
	}
	return "", false
}

// execResultBody is the finished result arm. Shell stream (field 14) is
// absent on purpose: a stdout chunk is not the call's result.
func execResultBody(fs []field) (kind string, body []byte, ok bool) {
	arms := []struct {
		n    int
		kind string
	}{
		{46, "pi-read"},
		{47, "pi-bash"},
		{48, "pi-edit"},
		{49, "pi-write"},
		{50, "pi-grep"},
		{51, "pi-find"},
		{52, "pi-ls"},
		{2, "shell"},
		{7, "read"},
		{4, "delete"},
		{11, "mcp"},
		{3, "write"},
		{5, "grep"},
		{8, "ls"},
	}
	for _, arm := range arms {
		raws := fieldBytes(fs, arm.n)
		if len(raws) == 0 {
			continue
		}
		return arm.kind, raws[0], true
	}
	return "", nil, false
}

func decodeExecBody(kind string, body []byte) (string, []ContentPart) {
	fs, err := parseFields(body)
	if err != nil {
		return "", nil
	}
	if raws := fieldBytes(fs, 1); len(raws) > 0 {
		return successText(kind, raws[0])
	}
	if raws := fieldBytes(fs, 2); len(raws) > 0 {
		return failText("error: ", raws[0]), nil
	}
	if raws := fieldBytes(fs, 3); len(raws) > 0 {
		return failText("rejected: ", raws[0]), nil
	}
	// A finished arm with no success, error, or rejected variant still
	// closes the call. Leaving it open waits until the client gives up.
	return "", nil
}

func failText(prefix string, msg []byte) string {
	fs, err := parseFields(msg)
	if err != nil {
		return strings.TrimSuffix(prefix, ": ")
	}
	for _, n := range []int{1, 2} {
		if s, ok := fieldString(fs, n); ok && strings.TrimSpace(s) != "" {
			return prefix + strings.TrimSpace(s)
		}
	}
	return strings.TrimSuffix(prefix, ": ")
}

func successText(kind string, msg []byte) (string, []ContentPart) {
	switch kind {
	case "shell":
		return shellSuccessText(msg), nil
	case "read":
		return readSuccessText(msg), nil
	case "delete":
		return deleteSuccessText(msg), nil
	case "mcp":
		return mcpSuccessText(msg)
	case "write":
		return writeSuccessText(msg), nil
	case "pi-edit":
		return piEditSuccessText(msg), nil
	default:
		return piOutputText(msg), nil
	}
}

func piOutputText(msg []byte) string {
	fs, err := parseFields(msg)
	if err != nil {
		return ""
	}
	s, _ := fieldString(fs, 1)
	return s
}

func piEditSuccessText(msg []byte) string {
	fs, err := parseFields(msg)
	if err != nil {
		return ""
	}
	out, _ := fieldString(fs, 1)
	diff, _ := fieldString(fs, 2)
	out = strings.TrimSpace(out)
	diff = strings.TrimSpace(diff)
	if out == "" {
		return diff
	}
	if diff == "" || strings.Contains(out, diff) {
		return out
	}
	return out + "\n" + diff
}

func shellSuccessText(msg []byte) string {
	fs, err := parseFields(msg)
	if err != nil {
		return ""
	}
	inter, _ := fieldString(fs, 10)
	stdout, _ := fieldString(fs, 5)
	stderr, _ := fieldString(fs, 6)
	body := strings.TrimSpace(inter)
	if body == "" {
		body = stdout
	}
	if stderr != "" {
		if body != "" {
			body += "\n"
		}
		body += stderr
	}
	if code, ok := fieldUint(fs, 3); ok && code != 0 {
		return fmt.Sprintf("exit %d\n%s", code, body)
	}
	return body
}

func readSuccessText(msg []byte) string {
	fs, err := parseFields(msg)
	if err != nil {
		return ""
	}
	if s, ok := fieldString(fs, 2); ok {
		return s
	}
	if data := fieldBytes(fs, 5); len(data) > 0 && len(data[0]) > 0 {
		return string(data[0])
	}
	return ""
}

func deleteSuccessText(msg []byte) string {
	fs, err := parseFields(msg)
	if err != nil {
		return "deleted"
	}
	path, _ := fieldString(fs, 1)
	path = strings.TrimSpace(path)
	if path == "" {
		return "deleted"
	}
	return "deleted " + path
}

func writeSuccessText(msg []byte) string {
	fs, err := parseFields(msg)
	if err != nil {
		return ""
	}
	if s, ok := fieldString(fs, 4); ok && strings.TrimSpace(s) != "" {
		return s
	}
	path, _ := fieldString(fs, 1)
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	return "wrote " + path
}

func mcpSuccessText(msg []byte) (string, []ContentPart) {
	fs, err := parseFields(msg)
	if err != nil {
		return "", nil
	}
	var b strings.Builder
	var images []ContentPart
	for _, item := range fieldBytes(fs, 1) {
		inf, err := parseFields(item)
		if err != nil {
			continue
		}
		if texts := fieldBytes(inf, 1); len(texts) > 0 {
			tf, err := parseFields(texts[0])
			if err == nil {
				if s, ok := fieldString(tf, 1); ok && s != "" {
					if b.Len() > 0 {
						b.WriteByte('\n')
					}
					b.WriteString(s)
				}
			}
		}
		if pics := fieldBytes(inf, 2); len(pics) > 0 {
			imf, err := parseFields(pics[0])
			if err != nil {
				continue
			}
			data := fieldBytes(imf, 1)
			mime, _ := fieldString(imf, 2)
			if len(data) == 0 {
				continue
			}
			part, ok := imageBytesPart(data[0], mime)
			if !ok {
				continue
			}
			if !hasImage(images, part.ImageURL) {
				images = append(images, part)
			}
		}
	}
	text := b.String()
	if n, ok := fieldUint(fs, 2); ok && n != 0 {
		if text != "" {
			text = "error: " + text
		} else if len(images) == 0 {
			text = "error"
		}
	}
	return text, images
}
