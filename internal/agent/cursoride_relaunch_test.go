package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/cursoride"
	"github.com/yetone/magpie/internal/edit"
)

func TestCursorDesktopRelaunchesOnlyAfterASettingsWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	prevRelaunch, prevLook := cursorIDEDoRelaunch, cursorIDELook
	var calls []string
	cursorIDEDoRelaunch = func(exe string) error {
		calls = append(calls, exe)
		return nil
	}
	const exe = "/Applications/Cursor.app/Contents/MacOS/Cursor"
	cursorIDELook = func() string { return exe }
	t.Cleanup(func() {
		cursorIDEStop()
		cursorIDEDoRelaunch = prevRelaunch
		cursorIDELook = prevLook
		cursorIDETrustMu.Lock()
		cursorIDERestarted = false
		cursorIDERestored = false
		cursorIDETrustRes = cursoride.TrustResult{}
		cursorIDETrustMu.Unlock()
	})

	a := cursorIDE(home)
	os.MkdirAll(filepath.Dir(a.Path), 0o755)
	os.WriteFile(a.Path, []byte("{\n  \"editor.fontSize\": 14,\n  \"http.proxy\": \"http://corp.example:8080\"\n}\n"), 0o644)
	if err := a.Field("provider").Set(""); err != nil {
		t.Fatal(err)
	}
	if v, _ := edit.GetJSON(a.Path, cursorIDEProxyKey); v != "http://corp.example:8080" {
		t.Fatalf("an unconnected disconnect rewrote the proxy as %q", v)
	}
	if len(calls) != 0 {
		t.Fatalf("disconnect with nothing connected relaunched: %v", calls)
	}
	beforeLog := cursoride.Recent()
	if err := a.Field("provider").Set(magpieID); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != exe {
		t.Fatalf("connect relaunch %v", calls)
	}
	if n := a.Notice(); !strings.Contains(n, "opened again") {
		t.Fatalf("notice %q", n)
	}
	if added := strings.TrimPrefix(cursoride.Recent(), beforeLog); !strings.Contains(added, "cursor relaunch opened") {
		t.Fatalf("connect diagnostics %q", added)
	}
	if err := a.Field("provider").Set(magpieID); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("a matching connect relaunched again: %v", calls)
	}
	KeepCursorIDE(t.Context())
	if len(calls) != 1 {
		t.Fatalf("keeping the proxy relaunched Cursor: %v", calls)
	}
	beforeLog = cursoride.Recent()
	if err := a.Field("provider").Set(""); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[1] != exe {
		t.Fatalf("disconnect relaunch %v", calls)
	}
	if v, _ := edit.GetJSON(a.Path, cursorIDEProxyKey); v != "http://corp.example:8080" {
		t.Fatalf("proxy restored as %q", v)
	}
	if _, ok := edit.GetJSON(a.Path, cursorIDEGatewayKey); ok {
		t.Fatal("magpie's gateway record was left behind")
	}
	if n := a.Notice(); !strings.Contains(n, "opened again") || strings.Contains(n, "pick a magpie model") {
		t.Fatalf("disconnect notice %q", n)
	}
	if added := strings.TrimPrefix(cursoride.Recent(), beforeLog); !strings.Contains(added, "cursor relaunch opened") {
		t.Fatalf("disconnect diagnostics %q", added)
	}
	if _, err := os.Stat(cursoride.CertFile(cursorIDECADir())); err != nil {
		t.Fatalf("certificate removed on disconnect: %v", err)
	}
	if err := a.Field("provider").Set(""); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("a disconnect that left settings alone relaunched Cursor: %v", calls)
	}

	os.WriteFile(a.Path, []byte("{}\n"), 0o644)
	calls = nil
	cursorIDEDoRelaunch = func(string) error { return errCursorIDENoRelaunch }
	if err := a.Field("provider").Set(magpieID); err != nil {
		t.Fatal(err)
	}
	if v, _ := edit.GetJSON(a.Path, cursorIDEProxyKey); v != cursoride.ProxyURL() {
		t.Fatalf("proxy %q", v)
	}
	if strings.Contains(a.Notice(), "opened again") || !strings.Contains(a.Notice(), "quit and reopen") {
		t.Fatalf("skipped relaunch notice %q", a.Notice())
	}
	calls = nil
	cursorIDEDoRelaunch = func(exe string) error {
		calls = append(calls, exe)
		return errCursorIDENoRelaunch
	}
	if err := a.Field("provider").Set(""); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != exe {
		t.Fatalf("skipped disconnect relaunch %v", calls)
	}
	if _, ok := edit.GetJSON(a.Path, cursorIDEProxyKey); ok {
		t.Fatal("skipped relaunch left the proxy settings")
	}
	if n := a.Notice(); strings.Contains(n, "opened again") || !strings.Contains(n, "quit and reopen") {
		t.Fatalf("skipped disconnect notice %q", n)
	}
}
