package cursoride

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

func TestDiagWritesTheConfigFile(t *testing.T) {
	Note("route ok model=aicodings-openai/gpt-6-astra path=AgentService/Run local")
	Note("chat start model=codex/gpt-5.6-luna bearer sk-secret-token")
	text := Recent()
	if !strings.Contains(text, "route ok model=aicodings-openai/gpt-6-astra") {
		t.Fatalf("recent log %q", text)
	}
	if strings.Contains(text, "sk-secret-token") || strings.Contains(strings.ToLower(text), "bearer sk") {
		t.Fatalf("token landed in the log: %q", text)
	}
	if !strings.Contains(text, "bearer [redacted]") {
		t.Fatalf("redaction %q", text)
	}
	b, err := os.ReadFile(DiagPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "route ok model=aicodings-openai/gpt-6-astra") {
		t.Fatalf("file %s missing the line:\n%s", DiagPath(), b)
	}
	if strings.Contains(DiagPath(), "cursor-ide"+string(os.PathSeparator)+"cursor-ide.log") == false {
		t.Fatalf("path %s", DiagPath())
	}
}

func TestDiagReachesTheProcessLogWhenDebug(t *testing.T) {
	t.Setenv("MAGPIE_DEBUG", "1")
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	Note("agent tool trace where=seal name=Read frags=1 placeholder=0 empty=false keys=path fill=none markers=none exec=run")
	got := buf.String()
	if !strings.Contains(got, "cursor-ide: agent tool trace where=seal") || !strings.Contains(got, "empty=false") || !strings.Contains(got, "exec=run") || !strings.Contains(got, "keys=path") {
		t.Fatalf("process log %q", got)
	}
}
