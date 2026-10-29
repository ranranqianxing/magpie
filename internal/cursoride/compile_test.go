package cursoride

import (
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

func TestCompileInvokeBecomesAGatewayChat(t *testing.T) {
	payload := invoke("inv-1",
		textMsg(4, "be brief"),
		userImage("look", "abc", "image/jpeg"),
		fileMsg("PNGSECRET", "image/png"),
		assistantCall("using a tool", "call_1", "read", `{"path":"a.go"}`),
		toolResult("call_1", "file contents"),
	)
	route := Route{Model: "oppo/deepseek-flash", Effort: "high", Fast: true, ContextTokens: 500_000}
	chat, err := CompileChat(route, "conv-9", payload)
	if err != nil {
		t.Fatal(err)
	}
	if chat.Model != route.Model || chat.Effort != "high" || chat.InvocationID != "inv-1" || !chat.Fast || chat.ContextTokens != 500_000 {
		t.Fatalf("%+v", chat)
	}
	if got := chat.Header()["X-Magpie-Session"]; got != "conv-9" {
		t.Fatalf("session = %q", got)
	}
	body, err := chat.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "PNGSECRET") || strings.Contains(string(body), "deepseek flash") {
		t.Fatalf("body leaked a file or used the display name: %s", body)
	}
	var got struct {
		Model           string `json:"model"`
		Stream          bool   `json:"stream"`
		ReasoningEffort string `json:"reasoning_effort"`
		Messages        []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "oppo/deepseek-flash" || !got.Stream || got.ReasoningEffort != "high" || len(got.Messages) != 5 {
		t.Fatalf("%+v", got)
	}
	if got.Messages[0].Role != "system" || string(got.Messages[0].Content) != `"be brief"` {
		t.Fatalf("system %+v", got.Messages[0])
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(got.Messages[1].Content, &parts); err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[0].Text != "look" || parts[1].ImageURL.URL != "data:image/jpeg;base64,abc" {
		t.Fatalf("parts %+v", parts)
	}
	if string(got.Messages[2].Content) != `"[attached file: image/png]"` {
		t.Fatalf("file = %s", got.Messages[2].Content)
	}
	call := got.Messages[3].ToolCalls
	if got.Messages[3].Role != "assistant" || len(call) != 1 || call[0].ID != "call_1" || call[0].Type != "function" || call[0].Function.Name != "read" || call[0].Function.Arguments != `{"path":"a.go"}` {
		t.Fatalf("call %+v", got.Messages[3])
	}
	if got.Messages[4].Role != "tool" || got.Messages[4].ToolCallID != "call_1" || string(got.Messages[4].Content) != `"file contents"` {
		t.Fatalf("tool %+v", got.Messages[4])
	}
}

func TestDecideSeparatesMagpieFromCursor(t *testing.T) {
	models := catalog()
	d, err := Decide(models, frameOf(runRequest("conv", "oppo/deepseek-flash[context=200k,reasoning=low,fast=false]", nil)))
	if err != nil || !d.Local || d.Route.Model != "oppo/deepseek-flash" || d.Route.Effort != "low" || d.ConversationID != "conv" {
		t.Fatalf("%+v err=%v", d, err)
	}
	d, err = Decide(models, runRequest("conv", "claude-4.5", nil))
	if err != nil || d.Local {
		t.Fatalf("foreign %+v err=%v", d, err)
	}
	d, err = Decide(models, invoke("inv", textMsg(1, "hi")))
	if err != nil || d.Local || d.Kind != KindInvoke {
		t.Fatalf("invoke %+v err=%v", d, err)
	}
	if _, err := Decide(models, []byte{0x0a, 0x05, 0x01}); err == nil {
		t.Fatal("truncated frame was accepted")
	}
	if _, err := CompileChat(Route{Model: "plain/m"}, "", runRequest("c", "plain/m", nil)); err == nil {
		t.Fatal("a run request compiled as a chat")
	}
	if _, err := CompileChat(Route{Model: "plain/m"}, "", invoke("inv")); err == nil {
		t.Fatal("empty invoke compiled")
	}
}

func TestUnknownInferenceRoleIsAnError(t *testing.T) {
	var msg wbuf
	msg.u32(1, 9)
	msg.str(2, "nope")
	if _, err := CompileChat(Route{Model: "plain/m"}, "", invoke("inv", msg)); err == nil {
		t.Fatal("role 9 compiled")
	}
}

func runRequest(conv, model string, params []Parameter) []byte {
	var modelMsg wbuf
	modelMsg.str(1, model)
	for _, p := range params {
		var one wbuf
		one.str(1, p.ID)
		one.str(2, p.Value)
		modelMsg.msg(3, one)
	}
	var run wbuf
	run.str(1, conv)
	run.msg(3, modelMsg)
	var client wbuf
	client.msg(1, run)
	return []byte(client)
}

func invoke(id string, messages ...wbuf) []byte {
	var req wbuf
	for _, m := range messages {
		req.msg(1, m)
	}
	var inv wbuf
	inv.str(1, id)
	if len(messages) > 0 {
		inv.msg(2, req)
	}
	var client wbuf
	client.msg(2, inv)
	return []byte(client)
}

func textMsg(role uint32, text string) wbuf {
	var b wbuf
	b.u32(1, role)
	b.str(2, text)
	return b
}

func userImage(text, data, mime string) wbuf {
	var textInner wbuf
	textInner.str(1, text)
	var textPart wbuf
	textPart.msg(1, textInner)
	var imgInner wbuf
	imgInner.str(1, data)
	imgInner.str(2, mime)
	var imgPart wbuf
	imgPart.msg(2, imgInner)
	var parts wbuf
	parts.msg(1, textPart)
	parts.msg(1, imgPart)
	var msg wbuf
	msg.u32(1, 1)
	msg.msg(3, parts)
	return msg
}

func fileMsg(data, media string) wbuf {
	var inner wbuf
	inner.str(1, data)
	inner.str(2, media)
	var part wbuf
	part.msg(3, inner)
	var parts wbuf
	parts.msg(1, part)
	var msg wbuf
	msg.u32(1, 1)
	msg.msg(3, parts)
	return msg
}

func assistantCall(text, id, name, args string) wbuf {
	var call wbuf
	call.str(1, id)
	call.str(2, name)
	call.str(4, args)
	var msg wbuf
	msg.u32(1, 2)
	msg.str(2, text)
	msg.msg(4, call)
	return msg
}

func toolResult(id, text string) wbuf {
	var val wbuf
	val.str(3, text)
	var part wbuf
	part.str(1, id)
	part.str(2, "read")
	part.msg(3, val)
	var content wbuf
	content.msg(1, part)
	var msg wbuf
	msg.u32(1, 3)
	msg.msg(6, content)
	return msg
}

func frameOf(p []byte) []byte {
	out := make([]byte, 5+len(p))
	binary.BigEndian.PutUint32(out[1:5], uint32(len(p)))
	copy(out[5:], p)
	return out
}
