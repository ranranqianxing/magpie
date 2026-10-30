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
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Image     json.RawMessage `json:"image"`
	MediaType string          `json:"mediaType"`
	MimeType  string          `json:"mimeType"`
	Data      string          `json:"data"`
	URL       string          `json:"url"`
	ImageURL  *struct {
		URL string `json:"url"`
	} `json:"image_url"`
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
	var images []ContentPart
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
		case "image", "image_url", "input_image":
			if img, ok := sdkImage(p); ok {
				images = append(images, img)
			}
		case "file":
			if img, ok := sdkImage(p); ok {
				images = append(images, img)
				break
			}
			s := p.Text
			if s == "" {
				s = strings.TrimSpace(p.Data)
			}
			if s != "" && utf8.ValidString(s) && !strings.ContainsRune(s, 0) {
				if text.Len() > 0 {
					text.WriteByte('\n')
				}
				text.WriteString(s)
			}
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
	if body != "" || len(calls) > 0 || len(images) > 0 {
		msg := ChatMessage{Role: chatRole(role), ToolCalls: calls}
		msg.Content = messageContent(body, images)
		out = append(out, msg)
	}
	out = append(out, tools...)
	return out, nil
}

// sdkImage reads one AI SDK image part. Cursor stores a data URL on
// image, with mediaType beside it. A bare base64 string is wrapped.
// A part with no image bytes is skipped.
func sdkImage(p sdkPart) (ContentPart, bool) {
	mime := p.MediaType
	if mime == "" {
		mime = p.MimeType
	}
	ref := ""
	switch {
	case p.ImageURL != nil && strings.TrimSpace(p.ImageURL.URL) != "":
		ref = strings.TrimSpace(p.ImageURL.URL)
	case strings.TrimSpace(p.URL) != "":
		ref = strings.TrimSpace(p.URL)
	default:
		ref = rawURL(p.Image)
		if ref == "" {
			ref = strings.TrimSpace(p.Data)
		}
	}
	if ref == "" {
		return ContentPart{}, false
	}
	// A file part is an image only when it says so. imageRef would
	// otherwise stamp an empty media type as image/png.
	if p.Type == "file" && !strings.HasPrefix(strings.ToLower(mime), "image/") && !strings.HasPrefix(ref, "data:image") {
		return ContentPart{}, false
	}
	ref = imageRef(ref, mime)
	if ref == "" {
		return ContentPart{}, false
	}
	return ContentPart{Type: "image_url", ImageURL: ref}, true
}

func rawURL(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return ""
		}
		return strings.TrimSpace(s)
	}
	if raw[0] == '{' {
		var obj struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(raw, &obj) != nil {
			return ""
		}
		return strings.TrimSpace(obj.URL)
	}
	return ""
}

func imageRef(data, mime string) string {
	data = strings.TrimSpace(data)
	if data == "" {
		return ""
	}
	if strings.HasPrefix(data, "data:") || strings.HasPrefix(data, "http://") || strings.HasPrefix(data, "https://") {
		return data
	}
	return imageDataURL(data, mime)
}

func messageContent(text string, images []ContentPart) any {
	if len(images) == 0 {
		if text == "" {
			return nil
		}
		return text
	}
	parts := make([]ContentPart, 0, 1+len(images))
	if text != "" {
		parts = append(parts, ContentPart{Type: "text", Text: text})
	}
	parts = append(parts, images...)
	return parts
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
func appendCurrent(msgs []ChatMessage, text string, attach userAttach) []ChatMessage {
	text = typedPrompt(text)
	if text == "" && attach.empty() {
		return msgs
	}
	if len(msgs) > 0 {
		last := &msgs[len(msgs)-1]
		if last.Role == "user" {
			if s, ok := messageText(*last); ok && text != "" && typedPrompt(s) == text {
				*last = mergeAttach(*last, attach)
				return msgs
			}
		}
	}
	content := messageContent(joinNote(text, attach.noteFor(text)), attach.images)
	if content == nil {
		return msgs
	}
	return append(msgs, ChatMessage{Role: "user", Content: content})
}

func messageText(m ChatMessage) (string, bool) {
	switch c := m.Content.(type) {
	case string:
		return c, true
	case []ContentPart:
		var b strings.Builder
		for _, p := range c {
			if p.Type != "text" || p.Text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(p.Text)
		}
		return b.String(), true
	default:
		return "", false
	}
}

func mergeAttach(m ChatMessage, attach userAttach) ChatMessage {
	if attach.empty() {
		return m
	}
	text, _ := messageText(m)
	body := joinNote(text, attach.noteFor(text))
	images := messageImages(m)
	for _, img := range attach.images {
		if !hasImage(images, img.ImageURL) {
			images = append(images, img)
		}
	}
	m.Content = messageContent(body, images)
	return m
}

func messageImages(m ChatMessage) []ContentPart {
	parts, ok := m.Content.([]ContentPart)
	if !ok {
		return nil
	}
	var out []ContentPart
	for _, p := range parts {
		if p.Type == "image_url" && p.ImageURL != "" {
			out = append(out, p)
		}
	}
	return out
}

func hasImage(parts []ContentPart, url string) bool {
	for _, p := range parts {
		if p.ImageURL == url {
			return true
		}
	}
	return false
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
