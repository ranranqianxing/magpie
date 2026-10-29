package cursoride

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

// userQuery is the tag Cursor wraps around what the user typed. The
// rest of a user blob (user_info, rules, open files) stays, because the
// model needs that context. Only the tag itself is removed.
var userQuery = regexp.MustCompile(`(?s)<user_query>\s*(.*?)\s*</user_query>`)

// replayConversation reads ConversationStateStructure. root_prompt_messages_json
// (field 1) is the prompt: each entry is either a JSON message or a
// 32-byte blob id. prefetched maps those ids to the bytes
// AgentRunRequest.pre_fetched_blobs carried. A missing blob is counted,
// not invented. turns (field 8) is not the prompt and is not read.
// A truncated message is an error. An empty state is an empty history.
func replayConversation(state []byte, prefetched map[string][]byte) (msgs []ChatMessage, missing int, err error) {
	if len(state) == 0 {
		return nil, 0, nil
	}
	fs, err := parseFields(state)
	if err != nil {
		return nil, 0, err
	}
	for _, raw := range fieldBytes(fs, 1) {
		body, miss := resolveBlob(raw, prefetched)
		missing += miss
		if body == nil {
			continue
		}
		if !looksJSON(body) {
			missing++
			continue
		}
		one, err := messagesFromJSON(body)
		if err != nil {
			return nil, missing, err
		}
		msgs = append(msgs, one...)
	}
	if summary := summaryText(fs, prefetched); summary != "" {
		msgs = append([]ChatMessage{{Role: "system", Content: summary}}, msgs...)
	}
	return msgs, missing, nil
}

// blobIndex reads AgentRunRequest.pre_fetched_blobs. Each entry's id is
// field 1 and its bytes are field 2. The id is stored both as hex and as
// the raw string, which is how a JSON blob and a sha256 id are looked up.
func blobIndex(entries [][]byte) map[string][]byte {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string][]byte, len(entries))
	for _, raw := range entries {
		fs, err := parseFields(raw)
		if err != nil {
			continue
		}
		id := fieldBytes(fs, 1)
		data := fieldBytes(fs, 2)
		if len(id) == 0 || len(data) == 0 || len(data[0]) == 0 {
			continue
		}
		putBlob(out, id[0], data[0])
	}
	return out
}

func putBlob(out map[string][]byte, id, data []byte) {
	out[hex.EncodeToString(id)] = data
	if utf8.Valid(id) {
		s := strings.TrimSpace(string(id))
		if s != "" {
			out[strings.ToLower(s)] = data
		}
	}
}

func resolveBlob(raw []byte, prefetched map[string][]byte) (body []byte, missing int) {
	if looksJSON(raw) {
		return raw, 0
	}
	if prefetched == nil {
		return nil, 1
	}
	if b, ok := prefetched[hex.EncodeToString(raw)]; ok {
		return b, 0
	}
	if utf8.Valid(raw) {
		if b, ok := prefetched[strings.ToLower(strings.TrimSpace(string(raw)))]; ok {
			return b, 0
		}
	}
	return nil, 1
}

func looksJSON(b []byte) bool {
	b = bytes.TrimSpace(b)
	return len(b) > 0 && (b[0] == '{' || b[0] == '[')
}

func summaryText(fs []field, prefetched map[string][]byte) string {
	raw := fieldBytes(fs, 6)
	if len(raw) == 0 {
		return ""
	}
	body, miss := resolveBlob(raw[0], prefetched)
	if miss > 0 || len(body) == 0 || !utf8.Valid(body) {
		return ""
	}
	if looksJSON(body) {
		msgs, err := messagesFromJSON(body)
		if err != nil || len(msgs) == 0 {
			return ""
		}
		if s, ok := msgs[0].Content.(string); ok {
			return s
		}
		return ""
	}
	s := strings.TrimSpace(string(body))
	if s == "" || strings.ContainsRune(s, 0) {
		return ""
	}
	return s
}

// messagesFromJSON reads one AI SDK message, or a JSON array of them.
// Tool calls and tool results become the gateway's tool messages.
// A body that is not JSON is an error: it was addressed as a message.
func messagesFromJSON(body []byte) ([]ChatMessage, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, nil
	}
	if body[0] == '[' {
		var raws []json.RawMessage
		if err := json.Unmarshal(body, &raws); err != nil {
			return nil, errf("history: %v", err)
		}
		var out []ChatMessage
		for _, raw := range raws {
			msgs, err := oneSDKMessage(raw)
			if err != nil {
				return nil, err
			}
			out = append(out, msgs...)
		}
		return out, nil
	}
	return oneSDKMessage(body)
}

type sdkMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type sdkPart struct {
	Type       string          `json:"type"`
	Text       string          `json:"text"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
	Result     json.RawMessage `json:"result"`
	IsError    bool            `json:"isError"`
}

func oneSDKMessage(raw []byte) ([]ChatMessage, error) {
	var m sdkMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, errf("history: %v", err)
	}
	role := strings.ToLower(strings.TrimSpace(m.Role))
	if role == "" {
		return nil, errf("history: message has no role")
	}
	if len(m.Content) == 0 || string(m.Content) == "null" {
		return nil, nil
	}
	if m.Content[0] == '"' {
		var s string
		if err := json.Unmarshal(m.Content, &s); err != nil {
			return nil, errf("history: %v", err)
		}
		s = presentText(role, s)
		if s == "" {
			return nil, nil
		}
		return []ChatMessage{{Role: chatRole(role), Content: s}}, nil
	}
	var parts []sdkPart
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return nil, errf("history: %v", err)
	}
	return partsToMessages(role, parts)
}

func partsToMessages(role string, parts []sdkPart) ([]ChatMessage, error) {
	var text strings.Builder
	var calls []ToolCall
	var tools []ChatMessage
	for _, p := range parts {
		switch p.Type {
		case "", "text":
			if p.Text != "" {
				if text.Len() > 0 {
					text.WriteByte('\n')
				}
				text.WriteString(p.Text)
			}
		case "reasoning", "redacted-reasoning":
		case "tool-call":
			args := strings.TrimSpace(string(p.Args))
			if args == "" || args == "null" {
				args = "{}"
			}
			id := p.ToolCallID
			if id == "" {
				id = "call-" + itoa(len(calls)+1)
			}
			calls = append(calls, ToolCall{ID: id, Name: p.ToolName, Arguments: args})
		case "tool-result":
			tools = append(tools, ChatMessage{
				Role: "tool", ToolCallID: p.ToolCallID, Content: toolResultString(p.Result, p.IsError),
			})
		}
	}
	var out []ChatMessage
	body := presentText(role, text.String())
	if body != "" || len(calls) > 0 {
		msg := ChatMessage{Role: chatRole(role), ToolCalls: calls}
		if body != "" {
			msg.Content = body
		}
		out = append(out, msg)
	}
	out = append(out, tools...)
	return out, nil
}

func presentText(role, s string) string {
	if role == "user" || role == "system" {
		s = unwrapUserText(s)
	}
	return strings.TrimSpace(s)
}

func unwrapUserText(s string) string {
	return userQuery.ReplaceAllString(s, "$1")
}

func chatRole(role string) string {
	switch role {
	case "system", "user", "assistant", "tool":
		return role
	default:
		return "user"
	}
}

func toolResultString(raw json.RawMessage, isError bool) string {
	raw = bytes.TrimSpace(raw)
	var s string
	if len(raw) == 0 || string(raw) == "null" {
		s = ""
	} else if raw[0] == '"' {
		_ = json.Unmarshal(raw, &s)
	} else {
		s = string(raw)
	}
	if isError && s != "" {
		return "error: " + s
	}
	if isError {
		return "error"
	}
	return s
}

// appendCurrent adds the user text from the run's action when history
// does not already end with that same typed prompt. Cursor sends the
// current prompt on the action and often repeats it as the last user
// turn, wrapped in user_info. The stored turn keeps that context; the
// action is only what was typed.
func appendCurrent(msgs []ChatMessage, text string) []ChatMessage {
	text = typedPrompt(text)
	if text == "" {
		return msgs
	}
	if len(msgs) > 0 {
		last := msgs[len(msgs)-1]
		if last.Role == "user" {
			if s, ok := last.Content.(string); ok && typedPrompt(s) == text {
				return msgs
			}
		}
	}
	return append(msgs, ChatMessage{Role: "user", Content: text})
}

// typedPrompt is the text the user typed. A stored user turn keeps
// user_info around it, and may still carry a user_query tag. The tail
// after user_info is that text. A turn with no such block is itself.
func typedPrompt(s string) string {
	s = strings.TrimSpace(unwrapUserText(s))
	const end = "</user_info>"
	if i := strings.LastIndex(s, end); i >= 0 {
		if tail := strings.TrimSpace(s[i+len(end):]); tail != "" {
			return tail
		}
	}
	return s
}
