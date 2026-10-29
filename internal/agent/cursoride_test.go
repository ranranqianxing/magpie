package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/cursoride"
	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
)

func TestCursorDesktopConnectsAndRestores(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Cleanup(cursorIDEStop)
	a := cursorIDE(home)
	if a.ID != cursoride.AgentID || a.Name != "Cursor" || !a.ListsModels || a.Launch != nil || a.shared != "" {
		t.Fatalf("id %s name %s lists %v launch %v shared %q", a.ID, a.Name, a.ListsModels, a.Launch != nil, a.shared)
	}
	if len(a.UA) != 0 {
		t.Fatalf("UA %v", a.UA)
	}
	opts := a.Field("provider").Options(nil)
	if len(opts) != 1 || opts[0].Value != magpieID {
		t.Fatalf("options %+v", opts)
	}
	cli := cursor(home)
	cliOpts := cli.Field("model").Options(nil)
	if len(cliOpts) != 1 || cliOpts[0].Value != "auto" {
		t.Fatalf("CLI options %+v", cliOpts)
	}
	if a.Field("provider").Get() != "" {
		t.Fatal("unset provider is not empty")
	}

	path := a.Path
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte("{\n  // keep me\n  \"editor.fontSize\": 14,\n  \"http.proxy\": \"http://corp.example:8080\",\n  \"http.noProxy\": \"localhost\"\n}\n"), 0o644)
	if err := a.Field("provider").Set(""); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if !strings.Contains(string(before), "http://corp.example:8080") || !strings.Contains(string(before), `"http.noProxy": "localhost"`) {
		t.Fatalf("disconnect with nothing connected rewrote settings:\n%s", before)
	}
	if err := a.Field("provider").Set(magpieID); err != nil {
		t.Fatal(err)
	}
	if a.Field("provider").Get() != magpieID || a.Check() != "" || !a.Wired() {
		t.Fatalf("get %q check %q wired %v", a.Field("provider").Get(), a.Check(), a.Wired())
	}
	b, _ := os.ReadFile(path)
	text := string(b)
	if !strings.Contains(text, "// keep me") || !strings.Contains(text, `"editor.fontSize": 14`) {
		t.Fatalf("a comment or the user's key went:\n%s", text)
	}
	for _, key := range []string{cursorIDEProxyKey, cursorIDEKerberosKey} {
		if v, _ := edit.GetJSON(path, key); v != cursoride.ProxyURL() {
			t.Fatalf("%s = %q", key, v)
		}
	}
	if v, _ := edit.GetJSON(path, cursorIDESupportKey); v != "on" {
		t.Fatalf("proxySupport %q", v)
	}
	if v, _ := edit.GetJSON(path, cursorIDEHTTP2Key); v != "true" {
		t.Fatalf("disableHttp2 %q", v)
	}
	if v, _ := edit.GetJSON(path, cursorIDECertsKey); v != "true" {
		t.Fatalf("certificates %q", v)
	}
	if _, ok := edit.GetJSON(path, cursorIDENoProxyKey); ok {
		t.Fatal("http.noProxy was left in place")
	}
	if v, _ := edit.GetJSON(path, cursorIDEGatewayKey); v != gateway.URL() {
		t.Fatalf("gateway record %q", v)
	}
	if !strings.Contains(a.Notice(), cursoride.CertFile(cursorIDECADir())) {
		t.Fatalf("notice %q", a.Notice())
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := a.Field("provider").Set(magpieID); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil || !st.ModTime().Equal(old) {
		t.Fatalf("a second connect wrote settings: %v", st.ModTime())
	}

	edit.SetJSON(path, edit.KV{Path: cursorIDEGatewayKey, Value: "http://127.0.0.1:9"})
	if a.Field("provider").Get() != magpieID {
		t.Fatal("proxy settings still match, but the row reads as disconnected")
	}
	if a.Check() == "" || a.Drift() == nil || a.Drift().Kind != "unwired" {
		t.Fatalf("check %q drift %+v", a.Check(), a.Drift())
	}
	if err := a.Reapply(); err != nil {
		t.Fatal(err)
	}
	if v, _ := edit.GetJSON(path, cursorIDEGatewayKey); v != gateway.URL() || a.Check() != "" {
		t.Fatalf("after reapply gateway %q check %q", v, a.Check())
	}

	if err := a.Field("provider").Set(""); err != nil {
		t.Fatal(err)
	}
	if a.Wired() || a.Field("provider").Get() != "" {
		t.Fatal("still connected")
	}
	b, _ = os.ReadFile(path)
	text = string(b)
	if !strings.Contains(text, "// keep me") || !strings.Contains(text, `"editor.fontSize": 14`) {
		t.Fatalf("restore lost the user's file:\n%s", text)
	}
	if v, _ := edit.GetJSON(path, "http\\.proxy"); v != "http://corp.example:8080" {
		t.Fatalf("proxy restored as %q\n%s", v, text)
	}
	if v, _ := edit.GetJSON(path, "http\\.noProxy"); v != "localhost" {
		t.Fatalf("noProxy restored as %q\n%s", v, text)
	}
	if _, ok := edit.GetJSON(path, cursorIDEGatewayKey); ok {
		t.Fatal("magpie's gateway record was left behind")
	}
}

func TestCursorDesktopPaths(t *testing.T) {
	getenv := func(k string) string {
		if k == "APPDATA" {
			return `C:\Users\me\AppData\Roaming`
		}
		if k == "XDG_CONFIG_HOME" {
			return "/home/me/.config"
		}
		return ""
	}
	if got, want := cursorIDEUserDir("darwin", "/Users/me", getenv), "/Users/me/Library/Application Support/Cursor/User"; got != want {
		t.Fatalf("darwin %s", got)
	}
	if got, want := cursorIDEUserDir("windows", `C:\Users\me`, getenv), filepath.Join(`C:\Users\me\AppData\Roaming`, "Cursor", "User"); got != want {
		t.Fatalf("windows %s", got)
	}
	if got, want := cursorIDEUserDir("linux", "/home/me", getenv), "/home/me/.config/Cursor/User"; got != want {
		t.Fatalf("linux %s", got)
	}
	empty := func(string) string { return "" }
	if got, want := cursorIDEUserDir("linux", "/home/me", empty), "/home/me/.config/Cursor/User"; got != want {
		t.Fatalf("linux default %s", got)
	}
	if got, want := cursorIDEUserDir("windows", `C:\Users\me`, empty), filepath.Join(`C:\Users\me`, "AppData", "Roaming", "Cursor", "User"); got != want {
		t.Fatalf("windows default %s", got)
	}
	if runtime.GOOS == "linux" {
		home := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
		if got := cursorIDE(home).Path; got != filepath.Join(home, ".config", "Cursor", "User", "settings.json") {
			t.Fatalf("constructed %s", got)
		}
	}
}

func TestCursorDesktopDetectsTheIDENotPrivateInference(t *testing.T) {
	wasLook, wasLocal := cursorIDELook, cursorLocalApp
	t.Cleanup(func() { cursorIDELook, cursorLocalApp = wasLook, wasLocal })
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	dir := cursorIDE(home).Dir
	cursorLocalApp = func() string { return "" }
	cursorIDELook = func() string { return "" }
	if cursorIDE(home).Detected() {
		t.Fatal("no install and no User folder is Cursor")
	}
	os.MkdirAll(dir, 0o755)
	if !cursorIDE(home).Detected() {
		t.Fatal("a User folder without Private Inference is not Cursor")
	}
	cursorLocalApp = func() string { return "/opt/Cursor Private Inference" }
	if cursorIDE(home).Detected() {
		t.Fatal("Private Inference's User folder is the ordinary IDE")
	}
	cursorIDELook = func() string { return "/opt/Cursor.app" }
	if !cursorIDE(home).Detected() {
		t.Fatal("an ordinary install beside Private Inference is not Cursor")
	}

	root := t.TempDir()
	writeAt(t, filepath.Join(root, "Cursor.app", "Contents", "Resources", "app", "product.json"), `{"nameShort":"Cursor","applicationName":"cursor"}`)
	writeAt(t, filepath.Join(root, "Private.app", "Contents", "Resources", "app", "product.json"), `{"nameShort":"Cursor Private Inference","applicationName":"cursor"}`)
	if exe := findCursorIDE([]string{root}); !strings.HasSuffix(exe, filepath.Join("Cursor.app", "Contents", "MacOS", "Cursor")) {
		t.Fatalf("found %q", exe)
	}
	img := filepath.Join(root, "Cursor-1.0-x86_64.AppImage")
	writeAt(t, img, "image")
	if !cursorIDEAppImage("Cursor-1.0-x86_64.AppImage") || cursorIDEAppImage("Cursor_Private_Inference-1.0-x86_64.AppImage") {
		t.Fatal("AppImage names")
	}
	if exe := cursorIDEAt(img); exe != img {
		t.Fatalf("appimage %q", exe)
	}
}

func TestCursorDesktopSyncKeepsHiddenModelsOut(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Cleanup(cursorIDEStop)
	if err := provider.Save(provider.Provider{ID: "deepseek", Name: "DeepSeek", Key: "k", Chat: "https://api.deepseek.com/v1", Models: []string{"pro", "flash"}}); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetHiddenModels(cursoride.AgentID, []string{"deepseek/flash"}); err != nil {
		t.Fatal(err)
	}
	a := cursorIDE(home)
	if err := a.Field("provider").Set(magpieID); err != nil {
		t.Fatal(err)
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	raw, ok := edit.GetJSONRaw(a.Path, cursorIDEModelsKey)
	if !ok || !strings.Contains(raw, "deepseek/pro") || strings.Contains(raw, "flash") {
		t.Fatalf("models %s", raw)
	}
	rows := cursoride.ForAgent(cursoride.AgentID)
	if len(rows) != 1 || rows[0].ID != "deepseek/pro" {
		t.Fatalf("injected %+v", rows)
	}
	before, _ := os.Stat(a.Path)
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(a.Path)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("a second sync wrote the model list again")
	}
}
