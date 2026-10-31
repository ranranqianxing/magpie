package cursoride

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Byok's Agent run is the inference local loop: BidiAppend logs append
// and chat start path=BidiAppend, and RunInferenceSSE is what Cursor
// concatenates into a Pi Read. where=seal is not on that path until the
// body has ended and one completing tool_call_part carries the path.
func TestBidiAppendSealsReadOntoTheCompletingToolPart(t *testing.T) {
	const path = "/tmp/magpie-p0-verify/README.md"
	const model = "deepseek/deepseek-v4.1-flash"
	obj := `{"path":"` + path + `"}`
	param := "I'll read it.\n<tool_call>{\"name\":\"Read\">\n<parameter name=\"path\">" + path + "</parameter>"
	cases := []struct {
		name string
		body string
		fill string
		skip bool
	}{
		{
			// finish_reason closes the call while the arguments are still
			// a prefix. The duplicate completion after it holds the object.
			name: "after-finish",
			fill: "message",
			body: `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_r","type":"function","function":{"name":"Read","arguments":"{\"path\": \""}}]}}]}` + "\n\n" +
				`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
				`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_r","function":{"name":"Read","arguments":"{}"}}]},"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_r","type":"function","function":{"name":"Read","arguments":` + obj + `}}]}}]}` + "\n\n" +
				"data: [DONE]\n\n",
		},
		{
			// DeepSeek's arguments are one JSON object, index 0.0.
			name: "object",
			fill: "none",
			body: `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0.0,"id":"call_r","type":"function","function":{"name":"Read","arguments":` + obj + `}}]},"finish_reason":"tool_calls"}]}` + "\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "same-chunk-message",
			fill: "message",
			body: `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_r","type":"function","function":{"name":"Read","arguments":"{}"}}]},"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_r","type":"function","function":{"name":"Read","arguments":` + obj + `}}]},"finish_reason":"tool_calls"}]}` + "\n\n" +
				`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_r","type":"function","function":{"name":"Read","arguments":"{}"}}]},"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_r","type":"function","function":{"name":"Read","arguments":` + obj + `}}]},"finish_reason":"tool_calls"}]}` + "\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "parameter",
			fill: "parameter",
			body: `data: {"choices":[{"delta":{"content":` + jsonQuote(param) + `}}]}` + "\n\n" +
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_r","function":{"name":"Read","arguments":"{}"}}]}}]}` + "\n\n" +
				`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name: "prose",
			skip: true,
			body: `data: {"choices":[{"delta":{"content":` + jsonQuote("I'll read "+path) + `}}]}` + "\n\n" +
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_r","function":{"name":"Read","arguments":"{}"}}]}}]}` + "\n\n" +
				`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
				"data: [DONE]\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MAGPIE_DEBUG", "1")
			var buf bytes.Buffer
			log.SetOutput(&buf)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })

			const conv = "11111111-1111-4111-8111-111111111111"
			gw := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, tc.body)
			})
			up := localHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("forwarded %s", r.URL.Path)
				w.WriteHeader(http.StatusOK)
			})
			dir := t.TempDir()
			p := &Proxy{
				Addr: "127.0.0.1:0", CADir: dir, Upstream: up.URL,
				Gateway: func() (string, string) { return gw.URL, "tok" },
				Models:  func() []Model { return []Model{{ID: model, Name: "Flash"}} },
			}
			addr, err := p.Start()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { p.Close() })
			client := cursorClient(t, strings.TrimPrefix(addr, "http://"), dir, "api2.cursor.sh")
			body := inferenceRound(t, client, conv, model, "read the readme")
			parts := toolPartsFromInference(t, body)
			logged := buf.String()
			if strings.Contains(logged, path) {
				t.Fatalf("process log kept the path:\n%s", logged)
			}
			if !strings.Contains(logged, "cursor-ide: append ") || !strings.Contains(logged, "chat start model="+model+" path=BidiAppend") || !strings.Contains(logged, "cursor-ide: proxy ") {
				t.Fatalf("live path was not BidiAppend:\n%s", logged)
			}
			if !strings.Contains(logged, "where=seal name=Read") || !strings.Contains(logged, "where=round sealed=1 exec=") {
				t.Fatalf("seal was silent on the live path:\n%s", logged)
			}
			if tc.skip {
				if len(parts) != 0 {
					t.Fatalf("empty Read was sent: %+v", parts)
				}
				if !strings.Contains(logged, "exec=skip") || !strings.Contains(logged, "where=round sealed=1 exec=0") {
					t.Fatalf("skip trace:\n%s", logged)
				}
				return
			}
			if len(parts) != 1 || !parts[0].complete || parts[0].name != "Read" || parts[0].id != "call_r" {
				t.Fatalf("parts %+v", parts)
			}
			var got map[string]any
			if json.Unmarshal([]byte(parts[0].args), &got) != nil || got["path"] != path {
				t.Fatalf("completing args %q", parts[0].args)
			}
			if !strings.Contains(logged, "fill="+tc.fill) || !strings.Contains(logged, "empty=false keys=path") || !strings.Contains(logged, "exec=written") || !strings.Contains(logged, "where=round sealed=1 exec=1") {
				t.Fatalf("trace:\n%s", logged)
			}
		})
	}
}

func inferenceRound(t *testing.T, client *http.Client, conv, model, text string) []byte {
	t.Helper()
	const reqID = "req-read"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api2.cursor.sh/aiserver.v1.InferenceService/RunInferenceSSE", bytes.NewReader(sseRequest(reqID)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/proto")
	sseResp := make(chan *http.Response, 1)
	sseErr := make(chan error, 1)
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			sseErr <- err
			return
		}
		sseResp <- resp
	}()
	postCursor(t, client, "/aiserver.v1.BidiService/BidiAppend", bidiAppend(reqID, runRequest(conv, model, nil)))
	postCursor(t, client, "/aiserver.v1.BidiService/BidiAppend", bidiAppend(reqID, invoke("inv-1", textMsg(1, text))))
	var resp *http.Response
	select {
	case resp = <-sseResp:
	case err := <-sseErr:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal("stream did not start")
	}
	defer resp.Body.Close()
	var stream bytes.Buffer
	tmp := make([]byte, 1024)
	finished := false
	for {
		n, rerr := resp.Body.Read(tmp)
		stream.Write(tmp[:n])
		if !finished && inferenceEnded(stream.Bytes()) {
			finished = true
			postCursor(t, client, "/aiserver.v1.BidiService/BidiAppend", bidiAppend(reqID, finishMsg()))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			t.Fatal(rerr)
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
	}
	if !finished {
		t.Fatal("invocation did not end")
	}
	return stream.Bytes()
}

func inferenceEnded(body []byte) bool {
	payloads, ok := connectPayloads(body)
	if !ok {
		return false
	}
	for _, p := range payloads {
		fs, err := parseFields(p)
		if err != nil {
			continue
		}
		if len(fieldBytes(fs, 4)) > 0 {
			return true
		}
	}
	return false
}

type inferenceToolPart struct {
	id, name, args string
	complete       bool
}

func toolPartsFromInference(t *testing.T, body []byte) []inferenceToolPart {
	t.Helper()
	payloads, ok := connectPayloads(body)
	if !ok {
		t.Fatalf("stream is not connect frames (%d bytes)", len(body))
	}
	var out []inferenceToolPart
	for _, p := range payloads {
		fs, err := parseFields(p)
		if err != nil {
			continue
		}
		for _, raw := range fieldBytes(fs, 3) {
			inv, err := parseFields(raw)
			if err != nil {
				continue
			}
			for _, stream := range fieldBytes(inv, 2) {
				sm, err := parseFields(stream)
				if err != nil {
					continue
				}
				for _, part := range fieldBytes(sm, 2) {
					pf, err := parseFields(part)
					if err != nil {
						t.Fatal(err)
					}
					id, _ := fieldString(pf, 1)
					name, _ := fieldString(pf, 2)
					args, _ := fieldString(pf, 3)
					_, complete := fieldUint(pf, 4)
					out = append(out, inferenceToolPart{id: id, name: name, args: args, complete: complete})
				}
			}
		}
	}
	return out
}
