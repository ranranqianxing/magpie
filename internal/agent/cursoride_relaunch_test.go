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
		cursorIDETrustRes = cursoride.TrustResult{}
		cursorIDETrustMu.Unlock()
	})

	a := cursorIDE(home)
	os.MkdirAll(filepath.Dir(a.Path), 0o755)
	os.WriteFile(a.Path, []byte("{}\n"), 0o644)
	if err := a.Field("provider").Set(magpieID); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0] != exe {
		t.Fatalf("connect relaunch %v", calls)
	}
	if n := a.Notice(); !strings.Contains(n, "opened again") {
		t.Fatalf("notice %q", n)
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
	if err := a.Field("provider").Set(""); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("disconnect relaunched Cursor: %v", calls)
	}
	if strings.Contains(a.Notice(), "opened again") {
		t.Fatalf("disconnect notice %q", a.Notice())
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
}
