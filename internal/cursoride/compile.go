package cursoride

import (
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
}

// ChatMessage is one turn in the gateway's chat body.
type ChatMessage struct {
	Role       string
	Content    any // string, or []ContentPart when the turn has an image
	ToolCalls  []ToolCall
	ToolCallID string
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
// named a level. It does not set a fast or context field: the gateway has
// no single field for those, and this slice does not invent one. Session
// is the conversation id, for the caller's X-Magpie-Session header.
type Chat struct {
	Model         string
	Effort        string
	Session       string
	InvocationID  string
	Messages      []ChatMessage
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
	ReasoningEffort string       `json:"reasoning_effort,omitempty"`
	Stream          bool         `json:"stream"`
}

type outMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []outTool       `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
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
	body := chatBody{Model: c.Model, Stream: true, ReasoningEffort: c.Effort}
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
	om := outMessage{Role: m.Role, ToolCallID: m.ToolCallID}
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
	if msg.Content == nil && len(msg.ToolCalls) == 0 {
		return nil, nil
	}
	return []ChatMessage{msg}, nil
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
	if mime == "" {
		mime = "image/png"
	}
	return "data:" + mime + ";base64," + data
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
	args, _ := fieldString(fs, 4)
	if id == "" {
		id = fmt.Sprintf("call-%d", index+1)
	}
	if args == "" {
		args = "{}"
	}
	return ToolCall{ID: id, Name: name, Arguments: args}, nil
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
		text := toolResultText(pf)
		out = append(out, ChatMessage{Role: "tool", ToolCallID: id, Content: text})
	}
	return out, nil
}

func toolResultText(fs []field) string {
	if raw := fieldBytes(fs, 3); len(raw) > 0 {
		if inner, err := parseFields(raw[len(raw)-1]); err == nil {
			if s, ok := fieldString(inner, 3); ok {
				return s
			}
		}
	}
	var b strings.Builder
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
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				if p.Type == "text" {
					b.WriteString(p.Text)
				} else {
					b.WriteString("[image]")
				}
			}
		}
	}
	return b.String()
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
		InvocationID: c.InvocationID, Messages: c.Messages,
		Fast: route.Fast, ContextTokens: route.ContextTokens,
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
