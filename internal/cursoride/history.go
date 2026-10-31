package cursoride

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
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
// not invented. turns (field 8) is the structured copy. It is used when
// the JSON prompt is missing blobs or produced no messages, and it
// yields more messages than that prompt. A complete JSON prompt is kept.
// A truncated message is an error. An empty state is an empty history.
func replayConversation(state []byte, prefetched map[string][]byte) (msgs []ChatMessage, missing int, err error) {
	if len(state) == 0 {
		return nil, 0, nil
	}
	fs, err := parseFields(state)
	if err != nil {
		return nil, 0, err
	}
	noted := 0
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
		one, err := messagesFromJSONNoted(body, &noted)
		if err != nil {
			return nil, missing, err
		}
		msgs = append(msgs, one...)
	}
	if noted > historyEmptyNotes {
		Note(fmt.Sprintf("agent history tools empty=%d logged=%d", noted, historyEmptyNotes))
	}
	// turns is the structured copy of the same conversation. It fills in
	// when the JSON prompt is missing blobs, which is how a long session
	// arrives before those blobs are prefetched. A complete JSON prompt
	// stays the prompt: turns are not appended beside it.
	if missing > 0 || len(msgs) == 0 {
		turns, turnMissing := replayTurns(fs, prefetched)
		if len(turns) > len(msgs) {
			Note(fmt.Sprintf("agent history source=turns messages=%d root=%d blobs-missing=%d", len(turns), len(msgs), missing))
			msgs, missing = turns, turnMissing
		}
	}
	if summary := summaryText(fs, prefetched); summary != "" {
		msgs = append([]ChatMessage{{Role: "system", Content: summary}}, msgs...)
	}
	if n := countChatImages(msgs); n > 0 || missing > 0 {
		Note(fmt.Sprintf("agent history messages=%d images=%d blobs-missing=%d", len(msgs), n, missing))
	}
	return msgs, missing, nil
}

func countChatImages(msgs []ChatMessage) int {
	n := 0
	for _, m := range msgs {
		n += len(messageImages(m))
	}
	return n
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
// historyEmptyNotes is how many empty tool-call parts one replay writes.
// The rest are counted. A long chain of Read {} would otherwise be the
// whole diagnostic file.
const historyEmptyNotes = 8

func messagesFromJSON(body []byte) ([]ChatMessage, error) {
	return messagesFromJSONNoted(body, nil)
}

func messagesFromJSONNoted(body []byte, noted *int) ([]ChatMessage, error) {
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
			msgs, err := oneSDKMessage(raw, noted)
			if err != nil {
				return nil, err
			}
			out = append(out, msgs...)
		}
		return out, nil
	}
	return oneSDKMessage(body, noted)
}

type sdkMessage struct {
	Role            string          `json:"role"`
	Content         json.RawMessage `json:"content"`
	ID              string          `json:"id"`
	ToolCallID      string          `json:"toolCallId"`
	ToolCallIDSnake string          `json:"tool_call_id"`
}

type sdkPart struct {
	Type       string          `json:"type"`
	Text       string          `json:"text"`
	Image      json.RawMessage `json:"image"`
	MediaType  string          `json:"mediaType"`
	MimeType   string          `json:"mimeType"`
	Data       string          `json:"data"`
	URL        string          `json:"url"`
	ImageURL   json.RawMessage `json:"image_url"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
	Input      json.RawMessage `json:"input"`
	Arguments  json.RawMessage `json:"arguments"`
	Parameters json.RawMessage `json:"parameters"`
	Result     json.RawMessage `json:"result"`
	Output     json.RawMessage `json:"output"`
	IsError    bool            `json:"isError"`
	raw        json.RawMessage
}

func oneSDKMessage(raw []byte, noted *int) ([]ChatMessage, error) {
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
		msg := ChatMessage{Role: chatRole(role), Content: s}
		if role == "tool" {
			msg.ToolCallID = messageToolID(m)
		}
		return []ChatMessage{msg}, nil
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(m.Content, &raws); err != nil {
		return nil, errf("history: %v", err)
	}
	parts := make([]sdkPart, 0, len(raws))
	for _, raw := range raws {
		var p sdkPart
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, errf("history: %v", err)
		}
		p.raw = raw
		parts = append(parts, p)
	}
	return partsToMessages(role, parts, messageToolID(m), noted)
}

func messageToolID(m sdkMessage) string {
	for _, s := range []string{m.ToolCallID, m.ToolCallIDSnake, m.ID} {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

func partsToMessages(role string, parts []sdkPart, toolID string, noted *int) ([]ChatMessage, error) {
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
			args, from := toolPartArgs(p)
			if callArgsEmpty(args) {
				if noted == nil || *noted < historyEmptyNotes {
					Note(fmt.Sprintf("agent history tool name=%s empty=true from=%s keys=%s", safeToolName(p.ToolName), from, partKeyList(p.raw)))
				}
				if noted != nil {
					*noted++
				}
			}
			id := p.ToolCallID
			if id == "" {
				id = "call-" + itoa(len(calls)+1)
			}
			calls = append(calls, ToolCall{ID: id, Name: p.ToolName, Arguments: args})
		case "tool-result":
			id := strings.TrimSpace(p.ToolCallID)
			if id == "" {
				id = toolID
			}
			body, imgs := toolResultContent(toolResultPayload(p), p.IsError)
			tools = append(tools, ChatMessage{
				Role: "tool", ToolCallID: id, Content: toolContent(body, imgs),
			})
		}
	}
	var out []ChatMessage
	body := presentText(role, text.String())
	// A tool turn whose only content is the result text (no tool-result
	// part) still has to name the call. The id is on the message.
	if role == "tool" && len(tools) == 0 && (body != "" || len(images) > 0) {
		return []ChatMessage{{
			Role: "tool", ToolCallID: toolID, Content: toolContent(body, images),
		}}, nil
	}
	if role != "tool" && (body != "" || len(calls) > 0 || len(images) > 0) {
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
	case imageRawRef(p.ImageURL) != "":
		ref = imageRawRef(p.ImageURL)
		if nested := imageRawMime(p.ImageURL); mime == "" {
			mime = nested
		}
	case strings.TrimSpace(p.URL) != "":
		ref = strings.TrimSpace(p.URL)
	default:
		ref = imageRawRef(p.Image)
		if nested := imageRawMime(p.Image); mime == "" {
			mime = nested
		}
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

func imageRawRef(raw json.RawMessage) string {
	ref, _ := imageRaw(raw)
	return ref
}

func imageRawMime(raw json.RawMessage) string {
	_, mime := imageRaw(raw)
	return mime
}

func imageRaw(raw json.RawMessage) (ref, mime string) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "", ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", ""
		}
		return strings.TrimSpace(s), ""
	}
	if raw[0] != '{' {
		return "", ""
	}
	var obj struct {
		URL       string `json:"url"`
		Data      string `json:"data"`
		Base64    string `json:"base64"`
		MediaType string `json:"mediaType"`
		MimeType  string `json:"mimeType"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return "", ""
	}
	mime = obj.MediaType
	if mime == "" {
		mime = obj.MimeType
	}
	switch {
	case strings.TrimSpace(obj.URL) != "":
		ref = strings.TrimSpace(obj.URL)
	case strings.TrimSpace(obj.Data) != "":
		ref = strings.TrimSpace(obj.Data)
	default:
		ref = strings.TrimSpace(obj.Base64)
	}
	return ref, mime
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

// toolPartArgs is the JSON object for one tool-call part. Cursor's
// stored messages use args. A part that only has input (or arguments,
// or parameters), or whose args object is {}, keeps that object instead
// of being replayed as an empty call. from is which field supplied it,
// or "none" when every one of them was missing or empty.
func toolPartArgs(p sdkPart) (args, from string) {
	for _, one := range []struct {
		name string
		raw  json.RawMessage
	}{
		{"args", p.Args},
		{"input", p.Input},
		{"arguments", p.Arguments},
		{"parameters", p.Parameters},
	} {
		s, ok := historyArgJSON(one.raw)
		if !ok || callArgsEmpty(s) {
			continue
		}
		return s, one.name
	}
	return "{}", "none"
}

func historyArgJSON(raw json.RawMessage) (string, bool) {
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
		if s == "" || !json.Valid([]byte(s)) || !strings.HasPrefix(s, "{") {
			return "", false
		}
		return s, true
	}
	if raw[0] == '{' && json.Valid(raw) {
		return string(raw), true
	}
	return "", false
}

func partKeyList(raw json.RawMessage) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || len(obj) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		if !safeKey(k) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 12 {
		keys = keys[:12]
	}
	if len(keys) == 0 {
		return "-"
	}
	return strings.Join(keys, ",")
}

// toolContent keeps an empty string. messageContent drops it, and a tool
// message with no content is not a result the next turn can pair.
func toolContent(text string, images []ContentPart) any {
	if len(images) == 0 {
		return text
	}
	return messageContent(text, images)
}

func toolResultPayload(p sdkPart) json.RawMessage {
	if s := bytes.TrimSpace(p.Result); len(s) > 0 && string(s) != "null" {
		return p.Result
	}
	return p.Output
}

// toolResultContent is the text and images inside a tool result. Cursor
// stores the body on result, and on output when that is the only field.
// An array of text and image parts stays images, so a later turn can
// still show the picture.
func toolResultContent(raw json.RawMessage, isError bool) (string, []ContentPart) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		if isError {
			return "error", nil
		}
		return "", nil
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return errorPrefix(s, isError), nil
	}
	if text, imgs, ok := toolResultParts(raw); ok {
		return errorPrefix(text, isError), imgs
	}
	return errorPrefix(string(raw), isError), nil
}

func errorPrefix(s string, isError bool) string {
	if !isError {
		return s
	}
	if strings.TrimSpace(s) == "" {
		return "error"
	}
	return "error: " + s
}

func toolResultParts(raw json.RawMessage) (string, []ContentPart, bool) {
	if len(raw) == 0 {
		return "", nil, false
	}
	if raw[0] == '[' {
		var raws []json.RawMessage
		if json.Unmarshal(raw, &raws) != nil {
			return "", nil, false
		}
		return partsPayload(raws)
	}
	if raw[0] != '{' {
		return "", nil, false
	}
	var obj struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		Value json.RawMessage `json:"value"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return "", nil, false
	}
	if obj.Type == "" && len(obj.Value) == 0 && obj.Text == "" {
		return "", nil, false
	}
	if obj.Type == "text" || obj.Type == "output_text" {
		if obj.Text != "" {
			return obj.Text, nil, true
		}
		if len(obj.Value) > 0 && obj.Value[0] == '"' {
			var s string
			if json.Unmarshal(obj.Value, &s) == nil {
				return s, nil, true
			}
		}
	}
	if obj.Type == "image" || obj.Type == "image_url" || obj.Type == "input_image" {
		var p sdkPart
		if json.Unmarshal(raw, &p) == nil {
			if img, ok := sdkImage(p); ok {
				return "", []ContentPart{img}, true
			}
		}
	}
	if len(obj.Value) > 0 && (obj.Value[0] == '[' || obj.Value[0] == '{') {
		if text, imgs, ok := toolResultParts(obj.Value); ok {
			return text, imgs, true
		}
	}
	if obj.Text != "" {
		return obj.Text, nil, true
	}
	return "", nil, false
}

func partsPayload(raws []json.RawMessage) (string, []ContentPart, bool) {
	var text strings.Builder
	var images []ContentPart
	saw := false
	for _, raw := range raws {
		var p sdkPart
		if json.Unmarshal(raw, &p) != nil {
			continue
		}
		switch p.Type {
		case "", "text", "output_text":
			if p.Text != "" {
				saw = true
				if text.Len() > 0 {
					text.WriteByte('\n')
				}
				text.WriteString(p.Text)
			}
		case "image", "image_url", "input_image", "file":
			if img, ok := sdkImage(p); ok {
				saw = true
				images = append(images, img)
			}
		}
	}
	if !saw {
		return "", nil, false
	}
	return text.String(), images, true
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
