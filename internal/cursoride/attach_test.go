package cursoride

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestAgentRunKeepsSelectedFileImageAndSelection(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x01}
	var img wbuf
	img.str(7, "image/png")
	img.str(8, string(png))
	var file wbuf
	file.str(1, "package main\nfunc main() {}\n")
	file.str(2, "/work/main.go")
	file.str(3, "main.go")
	var start, end wbuf
	start.u32(1, 10)
	end.u32(1, 12)
	var rng wbuf
	rng.msg(1, []byte(start))
	rng.msg(2, []byte(end))
	var sel wbuf
	sel.str(1, "unique selection line\n")
	sel.str(2, "/work/other.go")
	sel.msg(4, []byte(rng))
	var rule wbuf
	rule.str(1, "/work/.cursor/rules/style.mdc")
	rule.str(2, "use tabs")
	var attached wbuf
	attached.msg(1, []byte(rule))
	var ctx wbuf
	ctx.msg(1, []byte(img))
	ctx.str(3, "focus the handler")
	ctx.msg(4, []byte(file))
	ctx.msg(5, []byte(sel))
	ctx.msg(10, []byte(attached))
	var user wbuf
	user.str(1, "fix this")
	user.msg(3, []byte(ctx))
	var action wbuf
	action.msg(1, []byte(user))
	var uma wbuf
	uma.msg(1, []byte(action))
	var requested wbuf
	requested.str(1, "codex/gpt-5.6-luna")
	var run wbuf
	run.msg(2, []byte(uma))
	run.str(5, "conv-1")
	run.msg(9, []byte(requested))

	got, ok, err := readAgentClient(agentClient([]byte(run)))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.Text != "fix this" || got.MissingBlobs != 0 || len(got.Messages) != 1 {
		t.Fatalf("text=%q missing=%d messages=%d", got.Text, got.MissingBlobs, len(got.Messages))
	}
	body, err := (Chat{Model: got.ModelID, Messages: got.Messages}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	raw := string(body)
	wantURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	for _, s := range []string{
		"fix this",
		"package main",
		"main.go",
		"unique selection line",
		"lines 10-12",
		"use tabs",
		"focus the handler",
		wantURL,
	} {
		if !strings.Contains(raw, s) {
			t.Fatalf("chat body missing %q\n%s", s, raw)
		}
	}
}

func TestSelectedFileAlreadyInTheTurnIsNotRepeated(t *testing.T) {
	const pasted = "package main\nfunc main() {}\n"
	var file wbuf
	file.str(1, pasted)
	file.str(2, "/work/main.go")
	var ctx wbuf
	ctx.msg(4, []byte(file))
	body, err := json.Marshal(map[string]string{
		"role":    "user",
		"content": "<user_info>\nOS\n" + pasted + "/work/main.go\n</user_info>\nfix this",
	})
	if err != nil {
		t.Fatal(err)
	}
	user := body
	var state wbuf
	state.msg(1, user)
	var um wbuf
	um.str(1, "fix this")
	um.msg(3, []byte(ctx))
	var action wbuf
	action.msg(1, []byte(um))
	var uma wbuf
	uma.msg(1, []byte(action))
	var requested wbuf
	requested.str(1, "codex/gpt-5.6-luna")
	var run wbuf
	run.msg(1, []byte(state))
	run.msg(2, []byte(uma))
	run.msg(9, []byte(requested))
	got, ok, err := readAgentClient(agentClient([]byte(run)))
	if err != nil || !ok || len(got.Messages) != 1 {
		t.Fatalf("ok=%v err=%v messages=%d", ok, err, len(got.Messages))
	}
	s, ok := got.Messages[0].Content.(string)
	if !ok || strings.Contains(s, "Attached file") || !strings.Contains(s, "OS") || !strings.Contains(s, pasted) {
		t.Fatalf("content %#v", got.Messages[0].Content)
	}
}

func TestHistoryImageAndCurrentImageBothReachTheChat(t *testing.T) {
	prior := []byte(`{"role":"user","content":[{"type":"text","text":"see"},{"type":"image","image":"data:image/png;base64,aGk=","mediaType":"image/png"}]}`)
	var state wbuf
	state.msg(1, prior)
	png := []byte{0xff, 0xd8, 0xff, 0xd9}
	var img wbuf
	img.str(8, string(png))
	var ctx wbuf
	ctx.msg(1, []byte(img))
	var um wbuf
	um.str(1, "now")
	um.msg(3, []byte(ctx))
	var action wbuf
	action.msg(1, []byte(um))
	var uma wbuf
	uma.msg(1, []byte(action))
	var requested wbuf
	requested.str(1, "codex/gpt-5.6-luna")
	var run wbuf
	run.msg(1, []byte(state))
	run.msg(2, []byte(uma))
	run.msg(9, []byte(requested))
	got, ok, err := readAgentClient(agentClient([]byte(run)))
	if err != nil || !ok || len(got.Messages) != 2 {
		t.Fatalf("ok=%v err=%v messages=%d %+v", ok, err, len(got.Messages), got.Messages)
	}
	first, ok := got.Messages[0].Content.([]ContentPart)
	if !ok || len(first) != 2 || first[0].Text != "see" || first[1].ImageURL != "data:image/png;base64,aGk=" {
		t.Fatalf("history %#v", got.Messages[0].Content)
	}
	second, ok := got.Messages[1].Content.([]ContentPart)
	if !ok || len(second) != 2 || second[0].Text != "now" || !strings.HasPrefix(second[1].ImageURL, "data:image/jpeg;base64,") {
		t.Fatalf("current %#v", got.Messages[1].Content)
	}
}

func TestPrefetchedAndWrappedImagesAreDataURLs(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x02}
	png2 := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x03}
	id := make([]byte, 32)
	id[0] = 4
	var img wbuf
	img.str(7, "image/png")
	img.msg(1, id)
	var ctx wbuf
	ctx.msg(1, []byte(img))
	var wrapped wbuf
	wrapped.msg(2, png2)
	var wrappedImg wbuf
	wrappedImg.msg(9, []byte(wrapped))
	ctx.msg(1, []byte(wrappedImg))
	var um wbuf
	um.str(1, "look")
	um.msg(3, []byte(ctx))
	var action wbuf
	action.msg(1, []byte(um))
	var uma wbuf
	uma.msg(1, []byte(action))
	var entry wbuf
	entry.msg(1, id)
	entry.msg(2, png)
	var requested wbuf
	requested.str(1, "codex/gpt-5.6-luna")
	var run wbuf
	run.msg(2, []byte(uma))
	run.msg(9, []byte(requested))
	run.msg(17, []byte(entry))
	got, ok, err := readAgentClient(agentClient([]byte(run)))
	if err != nil || !ok || got.MissingBlobs != 0 || len(got.Messages) != 1 {
		t.Fatalf("ok=%v err=%v missing=%d n=%d", ok, err, got.MissingBlobs, len(got.Messages))
	}
	parts, ok := got.Messages[0].Content.([]ContentPart)
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	want2 := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png2)
	if !ok || len(parts) != 3 || parts[0].Text != "look" || parts[1].ImageURL != want || parts[2].ImageURL != want2 {
		t.Fatalf("%#v", got.Messages[0].Content)
	}
}

func TestHistoryFileTextAndBareImageBase64(t *testing.T) {
	raw := []byte(`{"role":"user","content":[{"type":"file","data":"package a","mediaType":"text/plain"},{"type":"image","image":"aGk=","mediaType":"image/png"}]}`)
	msgs, err := messagesFromJSON(raw)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("err=%v %+v", err, msgs)
	}
	parts, ok := msgs[0].Content.([]ContentPart)
	if !ok || len(parts) != 2 || parts[0].Text != "package a" || parts[1].ImageURL != "data:image/png;base64,aGk=" {
		t.Fatalf("%#v", msgs[0].Content)
	}
	plain, err := messagesFromJSON([]byte(`{"role":"user","content":[{"type":"file","data":"package a"}]}`))
	if err != nil || len(plain) != 1 || plain[0].Content.(string) != "package a" {
		t.Fatalf("untyped file %#v err=%v", plain, err)
	}
}

func TestBrokenSelectedContextKeepsTheTypedText(t *testing.T) {
	var um wbuf
	um.str(1, "fix this")
	um.msg(3, []byte{0x0a, 0x05})
	var action wbuf
	action.msg(1, []byte(um))
	var uma wbuf
	uma.msg(1, []byte(action))
	var requested wbuf
	requested.str(1, "codex/gpt-5.6-luna")
	var run wbuf
	run.msg(2, []byte(uma))
	run.msg(9, []byte(requested))
	got, ok, err := readAgentClient(agentClient([]byte(run)))
	if err != nil || !ok || len(got.Messages) != 1 {
		t.Fatalf("ok=%v err=%v messages=%d", ok, err, len(got.Messages))
	}
	if got.Messages[0].Content.(string) != "fix this" {
		t.Fatalf("%#v", got.Messages[0].Content)
	}
}

func TestImageBlobWithoutBytesIsNotInvented(t *testing.T) {
	id := make([]byte, 32)
	id[0] = 7
	var img wbuf
	img.str(7, "image/png")
	img.msg(1, id)
	var ctx wbuf
	ctx.msg(1, []byte(img))
	var um wbuf
	um.str(1, "look")
	um.msg(3, []byte(ctx))
	var action wbuf
	action.msg(1, []byte(um))
	var uma wbuf
	uma.msg(1, []byte(action))
	var requested wbuf
	requested.str(1, "codex/gpt-5.6-luna")
	var run wbuf
	run.msg(2, []byte(uma))
	run.msg(9, []byte(requested))
	got, ok, err := readAgentClient(agentClient([]byte(run)))
	if err != nil || !ok || got.MissingBlobs != 1 || len(got.Messages) != 1 {
		t.Fatalf("ok=%v err=%v missing=%d messages=%d", ok, err, got.MissingBlobs, len(got.Messages))
	}
	if got.Messages[0].Content.(string) != "look" {
		t.Fatalf("%#v", got.Messages[0].Content)
	}
}

func TestSamePromptKeepsHistoryAndAddsTheImage(t *testing.T) {
	user := []byte(`{"role":"user","content":"<user_info>\nOS\n</user_info>\nfix this"}`)
	var state wbuf
	state.msg(1, user)
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	var img wbuf
	img.msg(8, png)
	var ctx wbuf
	ctx.msg(1, []byte(img))
	var um wbuf
	um.str(1, "fix this")
	um.msg(3, []byte(ctx))
	var action wbuf
	action.msg(1, []byte(um))
	var uma wbuf
	uma.msg(1, []byte(action))
	var requested wbuf
	requested.str(1, "codex/gpt-5.6-luna")
	var run wbuf
	run.msg(1, []byte(state))
	run.msg(2, []byte(uma))
	run.msg(9, []byte(requested))
	got, ok, err := readAgentClient(agentClient([]byte(run)))
	if err != nil || !ok || len(got.Messages) != 1 {
		t.Fatalf("ok=%v err=%v n=%d", ok, err, len(got.Messages))
	}
	parts, ok := got.Messages[0].Content.([]ContentPart)
	if !ok || len(parts) != 2 || !strings.Contains(parts[0].Text, "OS") || !strings.Contains(parts[0].Text, "fix this") || !strings.HasPrefix(parts[1].ImageURL, "data:image/png;base64,") {
		t.Fatalf("%#v", got.Messages[0].Content)
	}
}
