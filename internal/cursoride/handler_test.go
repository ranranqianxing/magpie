package cursoride

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAvailableModelsMergesIntoCursorsList(t *testing.T) {
	upstream := frameOf(AvailableModels([]Model{{ID: "claude-4.5", Name: "Claude"}}))
	h := &Handler{
		Models: func() []Model { return []Model{deepseek()} },
		Upstream: func(r *http.Request, body []byte) (int, []byte, string, error) {
			if r.URL.Path != PathAvailableModels || string(body) != "request" {
				t.Fatalf("upstream saw %s %q", r.URL.Path, body)
			}
			return 200, upstream, "application/grpc", nil
		},
	}
	rec := post(t, h, PathAvailableModels, []byte("request"))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/grpc" {
		t.Fatalf("%d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	payload, framed, err := UnwrapUnary(rec.Body.Bytes())
	if err != nil || !framed {
		t.Fatalf("framed=%v err=%v", framed, err)
	}
	fs, err := parseFields(payload)
	if err != nil {
		t.Fatal(err)
	}
	names := fieldStrings(fs, 1)
	if len(names) != 2 || names[0] != "claude-4.5" || names[1] != "oppo/deepseek-flash" {
		t.Fatalf("names = %q", names)
	}
	if strings.Contains(string(payload), "oppoer") {
		t.Fatal("merged catalog leaked a host")
	}
}

func TestCatalogStandsAloneWhenCursorDoesNotAnswer(t *testing.T) {
	h := &Handler{
		Models: func() []Model { return []Model{deepseek()} },
		Upstream: func(*http.Request, []byte) (int, []byte, string, error) {
			return 503, []byte("cursor-down-sentinel"), "", nil
		},
	}
	rec := post(t, h, PathUsableModels, nil)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/proto" {
		t.Fatalf("%d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if strings.Contains(rec.Body.String(), "cursor-down-sentinel") {
		t.Fatal("kept Cursor's error body")
	}
	if !strings.Contains(rec.Body.String(), "oppo/deepseek-flash") || !strings.Contains(rec.Body.String(), LocalCredential) {
		t.Fatalf("local usable catalog = %q", rec.Body.Bytes())
	}
	alone := &Handler{Models: func() []Model { return []Model{deepseek()} }}
	rec = post(t, alone, PathDefaultModel, nil)
	fs, err := parseFields(rec.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := fieldString(fs, 1); got != "oppo/deepseek-flash" {
		t.Fatalf("default = %q", got)
	}
}

func TestDefaultModelKeepsCursorsChoice(t *testing.T) {
	var native wbuf
	native.str(1, "composer-1")
	h := &Handler{
		Models: func() []Model { return []Model{deepseek()} },
		Upstream: func(*http.Request, []byte) (int, []byte, string, error) {
			return 200, []byte(native), "application/proto", nil
		},
	}
	rec := post(t, h, PathDefaultModel, nil)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "oppo/deepseek-flash") {
		t.Fatal("replaced Cursor's default with a catalog id")
	}
	if !strings.Contains(rec.Body.String(), "composer-1") {
		t.Fatalf("dropped Cursor's default: %q", rec.Body.Bytes())
	}
	failed := &Handler{
		Models: func() []Model { return []Model{deepseek()} },
		Upstream: func(*http.Request, []byte) (int, []byte, string, error) {
			return 503, []byte("down"), "", nil
		},
	}
	rec = post(t, failed, PathDefaultModel, nil)
	if !strings.Contains(rec.Body.String(), "oppo/deepseek-flash") {
		t.Fatal("a failed default call did not fall back to the catalog")
	}
}

func TestCompressedCatalogIsNotMerged(t *testing.T) {
	bad := frameOf([]byte("native"))
	bad[0] = 1
	h := &Handler{
		Models:   func() []Model { return []Model{deepseek()} },
		Upstream: func(*http.Request, []byte) (int, []byte, string, error) { return 200, bad, "application/grpc", nil },
	}
	rec := post(t, h, PathUsableModelsAI, nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "oppo/deepseek-flash") {
		t.Fatal("answered with a catalog after a frame it could not merge")
	}
}

func TestCatalogRequestLimits(t *testing.T) {
	h := &Handler{Models: func() []Model { return []Model{deepseek()} }}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, PathAvailableModels, nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("get %d", rec.Code)
	}
	rec = post(t, h, "/v1/chat/completions", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("other path %d", rec.Code)
	}
	h.MaxBody = 4
	rec = post(t, h, PathDefaultModelNudge, []byte("12345"))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("large %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "oppo/deepseek-flash") {
		t.Fatal("oversized request was answered as an empty read")
	}
}

func TestModelsAreReadPerRequest(t *testing.T) {
	var n int
	h := &Handler{Models: func() []Model {
		n++
		return []Model{{ID: "p/m" + string(rune('0'+n)), Name: "M"}}
	}}
	first := post(t, h, PathDefaultModel, nil)
	second := post(t, h, PathDefaultModel, nil)
	if !strings.Contains(first.Body.String(), "p/m") || first.Body.String() == second.Body.String() {
		t.Fatalf("first %q second %q", first.Body.Bytes(), second.Body.Bytes())
	}
}

func post(t *testing.T, h http.Handler, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	h.ServeHTTP(rec, req)
	io.Copy(io.Discard, rec.Result().Body)
	return rec
}
