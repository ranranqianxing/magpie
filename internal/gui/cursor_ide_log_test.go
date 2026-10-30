package gui

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/cursoride"
)

func TestCursorIDELogIsTheSameTextAsTheFile(t *testing.T) {
	cursoride.Note("route ok model=workbuddy/deepseek-v4.1-flash path=AgentService/Run local")
	rec := httptest.NewRecorder()
	Handler(nil, nil).ServeHTTP(rec, httptest.NewRequest("GET", "/api/agents/cursor-ide/log", nil))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var out struct {
		Path string `json:"path"`
		Text string `json:"text"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("body %s", rec.Body)
	}
	if !strings.Contains(out.Text, "route ok model=workbuddy/deepseek-v4.1-flash") {
		t.Fatalf("text %q", out.Text)
	}
	if out.Text != cursoride.Recent() {
		t.Fatal("the page text is not the file text")
	}
	if !strings.HasSuffix(out.Path, "cursor-ide.log") || !strings.Contains(out.Path, "cursor-ide") {
		t.Fatalf("path %s", out.Path)
	}
}

func TestCursorIDELogWordsAreTranslated(t *testing.T) {
	b, err := os.ReadFile("assets/i18n.js")
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{
		"Diagnostics",
		"Copy all",
		"Reading the log…",
		"The log could not be read.",
		"No cursor-ide lines yet.",
		"Same lines as this file, for a terminal. A photo of the box is enough to share.",
		"Cursor was quit and opened again so it reads these proxy settings. Leave magpie running, then pick a magpie model in Cursor's model menu.",
		"Cursor was quit and opened again so it reads the proxy settings that were put back.",
		"Cursor reads its proxy settings when it starts — quit and reopen Cursor so the settings that were put back take effect.",
	}
	for _, lang := range []string{"zh: {", `"zh-TW": {`, "ja: {", "de: {"} {
		_, block, ok := strings.Cut(string(b), "\n  "+lang+"\n")
		if !ok {
			t.Fatalf("no %s block", lang)
		}
		block, _, _ = strings.Cut(block, "\n  },")
		for _, k := range keys {
			q, _ := json.Marshal(k)
			if !strings.Contains(block, "\n    "+string(q)+": ") {
				t.Errorf("%s missing %s", lang, q)
			}
		}
	}
}
