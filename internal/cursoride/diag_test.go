package cursoride

import (
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
