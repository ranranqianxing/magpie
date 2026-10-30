package gateway

import (
	"strings"
	"testing"
)

// DeepSeek sends a tool call's arguments as a JSON object, and sometimes
// as index 0.0. Either one used to fail the chunk, so the call never
// reached the client.
func TestChatDecoderKeepsObjectArguments(t *testing.T) {
	const path = "/tmp/magpie-p0-verify/README.md"
	d := &chatDecoder{}
	var name, args string
	err := d.decode(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0.0,"id":"call_r","type":"function","function":{"name":"Read","arguments":{"path":"`+path+`"}}}]},"finish_reason":"tool_calls"}]}`, func(ev Event) {
		switch ev.Kind {
		case KToolStart:
			name = ev.Name
		case KToolArgs:
			args += ev.Text
		}
	})
	if err != nil || name != "Read" || !strings.Contains(args, path) || strings.TrimSpace(args) == "{}" {
		t.Fatalf("name=%q args=%q err=%v", name, args, err)
	}
	// a string fragment is still that fragment
	d = &chatDecoder{}
	args = ""
	if err := d.decode(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"Read","arguments":""}}]}}]}`, func(Event) {}); err != nil {
		t.Fatal(err)
	}
	if err := d.decode(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"a.go\"}"}}]}}]}`, func(ev Event) {
		if ev.Kind == KToolArgs {
			args += ev.Text
		}
	}); err != nil {
		t.Fatal(err)
	}
	if args != `{"path":"a.go"}` {
		t.Fatalf("string args %q", args)
	}
}

func TestChatRequestKeepsStringArguments(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"Read","arguments":"{\"path\":\"a.go\"}"}}]}]}`)
	req, err := parseChat(body)
	if err != nil || len(req.Messages) != 1 || len(req.Messages[0].Parts) != 1 {
		t.Fatalf("%+v %v", req, err)
	}
	p := req.Messages[0].Parts[0]
	if p.Kind != ToolCall || p.Name != "Read" || string(p.Args) != `{"path":"a.go"}` {
		t.Fatalf("%+v", p)
	}
}
