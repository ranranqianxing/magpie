package cursoride

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Kind is which arm of a RunInference client message was set.
type Kind int

const (
	KindNone Kind = iota
	KindRun
	KindInvoke
	KindCancel
	KindFinish
	KindHeartbeat
)

// Client is a RunInference client message, decoded far enough to route
// it and to compile an invoke into chat messages. Later fields Cursor
// adds are skipped.
type Client struct {
	Kind           Kind
	ConversationID string
	InvocationID   string
	ModelID        string
	Params         []Parameter
	Messages       []ChatMessage
	Tools          []ToolSpec
	MaxTokens      int
}

// ChatMessage is one turn in the gateway's chat body.
type ChatMessage struct {
	Role       string
	Content    any // string, or []ContentPart when the turn has an image
	ToolCalls  []ToolCall
	ToolCallID string
	// Reasoning is assistant reasoning text the invoke carried.
	// It is the gateway's reasoning_content. A signature with no text
	// is not copied.
	Reasoning string
}

// ContentPart is one user part. An image's URL is a data URL.
type ContentPart struct {
	Type     string
	Text     string
	ImageURL string
}

// ToolCall is an assistant function call. Arguments are the raw JSON
// string Cursor sent, or "{}" when it sent none.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// Chat is a gateway /v1/chat/completions request for one local run.
//
// JSON sets model, messages, stream, and reasoning_effort when the picker
// named a level. Tools are set when the invoke or the agent run named
// tools. MaxTokens is the inference model config's output cap, when that
// request carried one. Fast and the context choice stay off the JSON: the
// gateway's max_tokens is an output cap, not Cursor's context window, and
// it has no field for the fast switch. Session is the conversation id,
// for the caller's X-Magpie-Session header.
type Chat struct {
	Model         string
	Effort        string
	Session       string
	InvocationID  string
	Messages      []ChatMessage
	Tools         []ToolSpec
	MaxTokens     int
	Fast          bool
	ContextTokens int
}

// Header is the session header, when the run named a conversation.
// The gateway token is not put here; the proxy adds gateway.TokenFor(AgentID).
func (c Chat) Header() map[string]string {
	if c.Session == "" {
		return nil
	}
	return map[string]string{"X-Magpie-Session": c.Session}
}

type chatBody struct {
	Model           string       `json:"model"`
	Messages        []outMessage `json:"messages"`
	Tools           []outToolDef `json:"tools,omitempty"`
	MaxTokens       int          `json:"max_tokens,omitempty"`
	ReasoningEffort string       `json:"reasoning_effort,omitempty"`
	Stream          bool         `json:"stream"`
}

type outToolDef struct {
	Type     string      `json:"type"`
	Function outToolFunc `json:"function"`
}

type outToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type outMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []outTool       `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Reasoning  string          `json:"reasoning_content,omitempty"`
}

type outTool struct {
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function outFunction `json:"function"`
}

type outFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type outPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

// JSON is the chat request body.
func (c Chat) JSON() ([]byte, error) {
	if c.Model == "" {
		return nil, fmt.Errorf("cursor ide: chat has no model")
	}
	if len(c.Messages) == 0 {
		return nil, fmt.Errorf("cursor ide: chat has no messages")
	}
	body := chatBody{Model: c.Model, Stream: true, ReasoningEffort: c.Effort, MaxTokens: c.MaxTokens}
	for _, t := range c.Tools {
		params := t.Parameters
		if len(params) == 0 || !json.Valid(params) {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		body.Tools = append(body.Tools, outToolDef{
			Type: "function",
			Function: outToolFunc{
				Name: t.Name, Description: t.Description, Parameters: params,
			},
		})
	}
	for _, m := range c.Messages {
		om, err := marshalMessage(m)
		if err != nil {
			return nil, err
		}
		body.Messages = append(body.Messages, om)
	}
	return json.Marshal(body)
}

func marshalMessage(m ChatMessage) (outMessage, error) {
	om := outMessage{Role: m.Role, ToolCallID: m.ToolCallID, Reasoning: m.Reasoning}
	for _, call := range m.ToolCalls {
		args := call.Arguments
		if args == "" {
			args = "{}"
		}
		om.ToolCalls = append(om.ToolCalls, outTool{
			ID: call.ID, Type: "function",
			Function: outFunction{Name: call.Name, Arguments: args},
		})
	}
	switch c := m.Content.(type) {
	case nil:
	case string:
		b, err := json.Marshal(c)
		if err != nil {
			return outMessage{}, err
		}
		om.Content = b
	case []ContentPart:
		parts := make([]outPart, 0, len(c))
		for _, p := range c {
			switch p.Type {
			case "text":
				parts = append(parts, outPart{Type: "text", Text: p.Text})
			case "image_url":
				parts = append(parts, outPart{Type: "image_url", ImageURL: &imageURL{URL: p.ImageURL}})
			default:
				return outMessage{}, fmt.Errorf("cursor ide: unknown content part %q", p.Type)
			}
		}
		b, err := json.Marshal(parts)
		if err != nil {
			return outMessage{}, err
		}
		om.Content = b
	default:
		return outMessage{}, fmt.Errorf("cursor ide: unknown message content")
	}
	return om, nil
}

// ReadClient decodes one RunInference client message. The payload is the
// protobuf, not the Connect frame; unwrap a frame with UnwrapUnary first.
func ReadClient(payload []byte) (Client, error) {
	fs, err := parseFields(payload)
	if err != nil {
		return Client{}, err
	}
	var c Client
	var body []byte
	for _, f := range fs {
		if f.wt != 2 {
			continue
		}
		switch f.num {
		case 1:
			c.Kind, body = KindRun, f.b
		case 2:
			c.Kind, body = KindInvoke, f.b
		case 3:
			c.Kind, body = KindCancel, f.b
		case 4:
			c.Kind, body = KindFinish, f.b
		case 5:
			c.Kind, body = KindHeartbeat, f.b
		}
	}
	switch c.Kind {
	case KindRun:
		return readRun(c, body)
	case KindInvoke:
		return readInvoke(c, body)
	case KindCancel:
		inner, err := parseFields(body)
		if err != nil {
			return Client{}, err
		}
		c.InvocationID, _ = fieldString(inner, 1)
		return c, nil
	default:
		return c, nil
	}
}

func readRun(c Client, body []byte) (Client, error) {
	fs, err := parseFields(body)
	if err != nil {
		return Client{}, err
	}
	c.ConversationID, _ = fieldString(fs, 1)
	models := fieldBytes(fs, 3)
	if len(models) == 0 {
		return c, nil
	}
	mf, err := parseFields(models[len(models)-1])
	if err != nil {
		return Client{}, err
	}
	c.ModelID, _ = fieldString(mf, 1)
	for _, raw := range fieldBytes(mf, 3) {
		pf, err := parseFields(raw)
		if err != nil {
			return Client{}, err
		}
		id, _ := fieldString(pf, 1)
		val, _ := fieldString(pf, 2)
		c.Params = append(c.Params, Parameter{ID: id, Value: val})
	}
	return c, nil
}

func readInvoke(c Client, body []byte) (Client, error) {
	fs, err := parseFields(body)
	if err != nil {
		return Client{}, err
	}
	c.InvocationID, _ = fieldString(fs, 1)
	reqs := fieldBytes(fs, 2)
	if len(reqs) == 0 {
		return c, nil
	}
	rf, err := parseFields(reqs[len(reqs)-1])
	if err != nil {
		return Client{}, err
	}
	for _, raw := range fieldBytes(rf, 1) {
		msgs, err := projectMessage(raw)
		if err != nil {
			return Client{}, err
		}
		c.Messages = append(c.Messages, msgs...)
	}
	tools, err := inferenceTools(rf)
	if err != nil {
		return Client{}, err
	}
	c.Tools = tools
	if cf := fieldBytes(rf, 4); len(cf) > 0 {
		cfs, err := parseFields(cf[0])
		if err != nil {
			return Client{}, err
		}
		if n, ok := fieldUint(cfs, 1); ok && n > 0 && n < 1<<31 {
			c.MaxTokens = int(n)
		}
	}
	return c, nil
}

func projectMessage(raw []byte) ([]ChatMessage, error) {
	fs, err := parseFields(raw)
	if err != nil {
		return nil, err
	}
	roleN, ok := fieldUint(fs, 1)
	if !ok {
		return nil, fmt.Errorf("cursor ide: inference message has no role")
	}
	var text string
	var parts []byte
	var toolBody []byte
	var haveText, haveParts, haveTool bool
	for _, f := range fs {
		switch f.num {
		case 2:
			if f.wt == 2 {
				text, haveText = string(f.b), true
			}
		case 3:
			if f.wt == 2 {
				parts, haveParts = f.b, true
			}
		case 6:
			if f.wt == 2 {
				toolBody, haveTool = f.b, true
			}
		}
	}
	if haveTool {
		return projectTool(toolBody)
	}
	role, ok := roleName(int(roleN))
	if !ok {
		return nil, fmt.Errorf("cursor ide: inference role %d", roleN)
	}
	msg := ChatMessage{Role: role}
	switch {
	case haveParts:
		content, err := projectParts(parts)
		if err != nil {
			return nil, err
		}
		msg.Content = content
	case haveText:
		msg.Content = text
	}
	for i, raw := range fieldBytes(fs, 4) {
		call, err := projectToolCall(raw, i)
		if err != nil {
			return nil, err
		}
		msg.ToolCalls = append(msg.ToolCalls, call)
	}
	if role == "assistant" {
		msg.Reasoning = reasoningText(fs)
	}
	if msg.Content == nil && len(msg.ToolCalls) == 0 && msg.Reasoning == "" {
		return nil, nil
	}
	return []ChatMessage{msg}, nil
}

// reasoningText is InferenceCoreMessage field 7. Each part's text is
// field 2. A redacted signature (field 4) with no text is not a thought
// the gateway can replay.
func reasoningText(fs []field) string {
	var b strings.Builder
	for _, raw := range fieldBytes(fs, 7) {
		pf, err := parseFields(raw)
		if err != nil {
			continue
		}
		s, _ := fieldString(pf, 2)
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(s)
	}
	return b.String()
}

func roleName(n int) (string, bool) {
	switch n {
	case 1:
		return "user", true
	case 2:
		return "assistant", true
	case 3:
		return "tool", true
	case 4:
		return "system", true
	default:
		return "", false
	}
}

func projectParts(raw []byte) (any, error) {
	fs, err := parseFields(raw)
	if err != nil {
		return nil, err
	}
	var parts []ContentPart
	for _, one := range fieldBytes(fs, 1) {
		pf, err := parseFields(one)
		if err != nil {
			return nil, err
		}
		var part []byte
		var kind int
		for _, f := range pf {
			if f.wt == 2 && (f.num == 1 || f.num == 2 || f.num == 3) {
				part, kind = f.b, f.num
			}
		}
		if kind == 0 {
			continue
		}
		inner, err := parseFields(part)
		if err != nil {
			return nil, err
		}
		switch kind {
		case 1:
			s, _ := fieldString(inner, 1)
			parts = append(parts, ContentPart{Type: "text", Text: s})
		case 2:
			data, _ := fieldString(inner, 1)
			mime, _ := fieldString(inner, 2)
			parts = append(parts, ContentPart{Type: "image_url", ImageURL: imageDataURL(data, mime)})
		case 3:
			data, _ := fieldString(inner, 1)
			media, _ := fieldString(inner, 2)
			if part, ok := fileImagePart(data, media); ok {
				parts = append(parts, part)
				break
			}
			parts = append(parts, ContentPart{Type: "text", Text: fileText(data, media)})
		}
	}
	if len(parts) == 0 {
		return nil, nil
	}
	if len(parts) == 1 && parts[0].Type == "text" {
		return parts[0].Text, nil
	}
	return parts, nil
}

func imageDataURL(data, mime string) string {
	if strings.HasPrefix(data, "data:") {
		return data
	}
	// An image part sometimes carries the file bytes rather than base64.
	// A string that is already base64 does not sniff as an image, so it
	// is left as the payload.
	if sniff := sniffImageMime([]byte(data)); sniff != "" {
		if !strings.HasPrefix(strings.ToLower(mime), "image/") {
			mime = sniff
		}
		data = base64.StdEncoding.EncodeToString([]byte(data))
	}
	if mime == "" {
		mime = "image/png"
	}
	return "data:" + mime + ";base64," + data
}

// fileImagePart is an inference file part whose bytes are an image.
// A part only labeled image/*, with bytes that are not an image, stays
// the media-type note: that label is not the picture.
func fileImagePart(data, media string) (ContentPart, bool) {
	if sniffImageMime([]byte(data)) == "" {
		return ContentPart{}, false
	}
	return ContentPart{Type: "image_url", ImageURL: imageDataURL(data, media)}, true
}

func fileText(data, media string) string {
	if strings.HasPrefix(media, "text/") || media == "application/json" {
		return data
	}
	if media == "" {
		media = "file"
	}
	return "[attached file: " + media + "]"
}

func projectToolCall(raw []byte, index int) (ToolCall, error) {
	fs, err := parseFields(raw)
	if err != nil {
		return ToolCall{}, err
	}
	id, _ := fieldString(fs, 1)
	name, _ := fieldString(fs, 2)
	if id == "" {
		id = fmt.Sprintf("call-%d", index+1)
	}
	return ToolCall{ID: id, Name: name, Arguments: toolCallArgs(fs)}, nil
}

// toolCallArgs is InferenceToolCall. Field 3 is the protobuf Struct the
// official inference server reads. Field 4 is the raw JSON string some
// invokes send beside it. A string that names fields wins, so an older
// invoke that only set field 4 stays byte-for-byte. "{}" does not hide
// the struct: that placeholder is how a call arrives before the object.
func toolCallArgs(fs []field) string {
	raw, _ := fieldString(fs, 4)
	structured := ""
	if blobs := fieldBytes(fs, 3); len(blobs) > 0 {
		if obj, err := decodeStruct(blobs[0]); err == nil {
			if s, err := jsonCompact(obj); err == nil {
				structured = s
			}
		} else if looksJSON(blobs[0]) && json.Valid(blobs[0]) {
			structured = string(bytesTrim(blobs[0]))
		}
	}
	if toolArgsUseful(raw) {
		return strings.TrimSpace(raw)
	}
	if toolArgsUseful(structured) {
		return structured
	}
	if strings.TrimSpace(raw) != "" {
		return strings.TrimSpace(raw)
	}
	if strings.TrimSpace(structured) != "" {
		return strings.TrimSpace(structured)
	}
	return "{}"
}

func bytesTrim(b []byte) string {
	return strings.TrimSpace(string(b))
}

// toolArgsUseful is JSON the model can read as arguments. "{}" and
// "null" are placeholders. An array or a non-empty object is the call.
func toolArgsUseful(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || s == "{}" || s == "null" || !json.Valid([]byte(s)) {
		return false
	}
	if s[0] != '{' {
		return true
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(s), &obj) != nil {
		return false
	}
	return len(obj) > 0
}

func projectTool(raw []byte) ([]ChatMessage, error) {
	fs, err := parseFields(raw)
	if err != nil {
		return nil, err
	}
	var out []ChatMessage
	for _, part := range fieldBytes(fs, 1) {
		pf, err := parseFields(part)
		if err != nil {
			return nil, err
		}
		id, _ := fieldString(pf, 1)
		text, images := toolResultBody(pf)
		out = append(out, ChatMessage{Role: "tool", ToolCallID: id, Content: toolContent(text, images)})
	}
	return out, nil
}

func toolResultBody(fs []field) (string, []ContentPart) {
	if raw := fieldBytes(fs, 3); len(raw) > 0 {
		if text, images, ok := toolResultValue(raw[len(raw)-1]); ok {
			return text, images
		}
	}
	var b strings.Builder
	var images []ContentPart
	for _, exp := range fieldBytes(fs, 5) {
		content, err := projectParts(exp)
		if err != nil {
			continue
		}
		switch c := content.(type) {
		case string:
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(c)
		case []ContentPart:
			for _, p := range c {
				if p.Type == "image_url" && p.ImageURL != "" {
					if !hasImage(images, p.ImageURL) {
						images = append(images, p)
					}
					continue
				}
				if p.Type == "text" && p.Text != "" {
					if b.Len() > 0 {
						b.WriteByte('\n')
					}
					b.WriteString(p.Text)
				}
			}
		}
	}
	return b.String(), images
}

// toolResultValue is InferenceToolResultPart.result, a protobuf Value.
// A string stays that string. A list of text and image parts stays text
// and images. Any other value is its JSON, which is the object an
// official server would hand the model. A value that does not decode
// is not a result: the caller tries the older parts field.
func toolResultValue(raw []byte) (string, []ContentPart, bool) {
	v, err := decodeValue(raw)
	if err != nil || v == nil {
		return "", nil, false
	}
	switch t := v.(type) {
	case string:
		return t, nil, true
	case []any:
		if text, images, ok := contentList(t); ok {
			return text, images, true
		}
	case map[string]any:
		if text, images, ok := contentList([]any{t}); ok {
			return text, images, true
		}
	}
	s, err := jsonCompact(v)
	if err != nil {
		return "", nil, false
	}
	return s, nil, true
}

// jsonCompact is one JSON value with HTML left as characters. The chat
// body encodes the string again, so a '<' that was already escaped here
// would reach the model as the letters \u003c.
func jsonCompact(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

// contentList is a Value list of {type,text} or image parts. A list
// that is not that shape is refused so the caller keeps the JSON.
func contentList(list []any) (string, []ContentPart, bool) {
	if len(list) == 0 {
		return "", nil, false
	}
	var b strings.Builder
	var images []ContentPart
	for _, one := range list {
		obj, ok := one.(map[string]any)
		if !ok {
			return "", nil, false
		}
		typ, _ := obj["type"].(string)
		switch typ {
		case "text":
			s, _ := obj["text"].(string)
			if s == "" {
				return "", nil, false
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(s)
		case "image", "image_url":
			url := imageFromValue(obj)
			if url == "" {
				return "", nil, false
			}
			if !hasImage(images, url) {
				images = append(images, ContentPart{Type: "image_url", ImageURL: url})
			}
		default:
			return "", nil, false
		}
	}
	if b.Len() == 0 && len(images) == 0 {
		return "", nil, false
	}
	return b.String(), images, true
}

func imageFromValue(obj map[string]any) string {
	if s, ok := obj["url"].(string); ok && s != "" {
		return imageDataURL(s, "")
	}
	if img, ok := obj["image_url"].(map[string]any); ok {
		if s, ok := img["url"].(string); ok && s != "" {
			return imageDataURL(s, "")
		}
	}
	data, _ := obj["data"].(string)
	if data == "" {
		return ""
	}
	mime, _ := obj["mime_type"].(string)
	if mime == "" {
		mime, _ = obj["media_type"].(string)
	}
	return imageDataURL(data, mime)
}

// CompileChat builds the gateway request for an invoke that belongs to
// route. payload is the RunInference client message that carries
// invoke_model. session is the conversation id from the run request.
func CompileChat(route Route, session string, payload []byte) (Chat, error) {
	plain, _, err := UnwrapUnary(payload)
	if err != nil {
		return Chat{}, err
	}
	c, err := ReadClient(plain)
	if err != nil {
		return Chat{}, err
	}
	if c.Kind != KindInvoke {
		return Chat{}, fmt.Errorf("cursor ide: expected invoke_model")
	}
	if len(c.Messages) == 0 {
		return Chat{}, fmt.Errorf("cursor ide: invoke has no messages")
	}
	return Chat{
		Model: route.Model, Effort: route.Effort, Session: session,
		InvocationID: c.InvocationID, Messages: c.Messages, Tools: c.Tools,
		MaxTokens: c.MaxTokens, Fast: route.Fast, ContextTokens: route.ContextTokens,
	}, nil
}

// Decide says whether a run_request names a magpie model. Any other
// client message is not local. A truncated payload is an error, not a
// request to forward: forwarding bytes that did not parse would hide a
// broken frame.
func Decide(models []Model, payload []byte) (Decision, error) {
	plain, _, err := UnwrapUnary(payload)
	if err != nil {
		return Decision{}, err
	}
	c, err := ReadClient(plain)
	if err != nil {
		return Decision{}, err
	}
	d := Decision{Kind: c.Kind, ConversationID: c.ConversationID, InvocationID: c.InvocationID}
	if c.Kind != KindRun {
		return d, nil
	}
	route, ok, err := RouteModel(models, c.ModelID, c.Params)
	if err != nil {
		return Decision{}, err
	}
	d.Local = ok
	d.Route = route
	return d, nil
}

// Decision is what a proxy does with one client message.
// Local means the run names a magpie model and must not be forwarded to
// Cursor's backend. Route is set only then.
type Decision struct {
	Kind           Kind
	Local          bool
	ConversationID string
	InvocationID   string
	Route          Route
}
