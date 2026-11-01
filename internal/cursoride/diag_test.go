package cursoride

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
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

// TestDiagCompactDoesNotAppendPendingTwice forces the log over its
// cap while more Notes land. Those Notes are already in the page
// buffer, so the rewrite writes them. They used to stay queued and
// get appended again. The first append waits until the oversized
// batch is queued, and the compact waits until the extra Notes have
// landed. The 5s bounds are only those waits failing to start. The
// extra lines are few enough that appending them again would not
// cross the cap a second time and hide the duplicate.
func TestDiagCompactDoesNotAppendPendingTwice(t *testing.T) {
	t.Setenv(diagSandbox, "")
	// Start from an empty log so the extra lines, appended a second
	// time, stay under the cap. A second compact would rewrite the
	// file from memory and hide the duplicate.
	diagMu.Lock()
	diagLines = nil
	diagPending = nil
	diagMu.Unlock()
	_ = os.Remove(DiagPath())
	appendStarted := make(chan struct{})
	appendRelease := make(chan struct{})
	compactStarted := make(chan struct{})
	compactRelease := make(chan struct{})
	var appendOnce, compactOnce sync.Once
	diagBeforeAppend = func() {
		appendOnce.Do(func() {
			close(appendStarted)
			<-appendRelease
		})
	}
	diagBeforeCompact = func() {
		compactOnce.Do(func() {
			close(compactStarted)
			<-compactRelease
		})
	}
	t.Cleanup(func() {
		diagBeforeAppend = nil
		diagBeforeCompact = nil
		for _, ch := range []chan struct{}{appendRelease, compactRelease} {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	})
	msg := strings.Repeat("m", 120)
	Note("compact-prime " + msg)
	select {
	case <-appendStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("append did not start")
	}
	for i := 0; i < 500; i++ {
		Note(fmt.Sprintf("compact-fill-%d %s", i, msg))
	}
	close(appendRelease)
	select {
	case <-compactStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("compact did not start")
	}
	const extras = 8
	for i := 0; i < extras; i++ {
		Note(fmt.Sprintf("compact-extra-%d %s", i, msg))
	}
	close(compactRelease)
	diagWG.Wait()
	b, err := os.ReadFile(DiagPath())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		seen[line]++
		if seen[line] > 1 {
			t.Fatalf("duplicate line %q", line)
		}
	}
	raw := string(b)
	for i := 0; i < extras; i++ {
		want := fmt.Sprintf("compact-extra-%d %s", i, msg)
		n := strings.Count(raw, want)
		if n != 1 {
			t.Fatalf("extra %d appears %d times", i, n)
		}
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
