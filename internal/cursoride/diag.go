package cursoride

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/appdir"
)

// The diagnostic log is one file beside the CA, and the same lines the
// Agents page shows for cursor-ide. A photo of that box is the shareable
// copy; the file is there for a terminal. It never contains a gateway
// token or a private key.

const (
	diagFileName  = "cursor-ide.log"
	diagKeepLines = 200
	diagMaxBytes  = 48 << 10
)

var diagMu sync.Mutex
var diagLines []string
var diagOnce sync.Once
var diagPending []string
var diagWG sync.WaitGroup
var diagCond = sync.NewCond(&diagMu)
var diagWriter sync.Once

// diagSandbox is set by the test harness. Writes stay on the caller
// there so a test can read the file when Note returns, and so the
// writer does not still have the file open when the temporary home is
// removed. A real run appends off the request goroutine: a full log
// used to be rewritten on that goroutine before the gateway was asked.
const diagSandbox = "MAGPIE_TEST_SANDBOX"

// DiagPath is the log file. Installed, that is under the user's config
// folder (not Cursor's %APPDATA%\Cursor): ~/.config/magpie/cursor-ide
// on every system, $XDG_CONFIG_HOME/magpie/cursor-ide when that is set,
// or the data folder beside magpie when the run is portable.
func DiagPath() string {
	return filepath.Join(appdir.Config(), "cursor-ide", diagFileName)
}

// Note appends one timestamped line to the buffer the Agents page reads
// and to the log file. Newlines are folded. A bearer token is not kept.
// MAGPIE_DEBUG also prints the line. The file write is not on the caller
// outside the test sandbox, so a request does not wait on it.
func Note(msg string) {
	msg = sanitizeDiag(msg)
	if msg == "" {
		return
	}
	line := time.Now().Format("2006-01-02 15:04:05") + " " + msg
	loadDiag()
	diagMu.Lock()
	diagLines = append(diagLines, line)
	if len(diagLines) > diagKeepLines {
		diagLines = append([]string(nil), diagLines[len(diagLines)-diagKeepLines:]...)
	}
	diagMu.Unlock()
	// The process log is what MAGPIE_DEBUG tails. It stays on the caller
	// so a test that set the logger sees the line before Note returns.
	// The file does not: outside the sandbox it is appended off this
	// goroutine, after the line is already in the page buffer.
	if os.Getenv("MAGPIE_DEBUG") != "" {
		log.Printf("cursor-ide: %s", msg)
	}
	if os.Getenv(diagSandbox) != "" {
		appendDiagBatch([]string{line})
		return
	}
	ensureDiagWriter()
	diagMu.Lock()
	diagPending = append(diagPending, line)
	diagWG.Add(1)
	diagCond.Signal()
	diagMu.Unlock()
}

// Recent is the log the Agents page shows. It is the lines noted in this
// process, seeded from the file the first time, so a line is visible
// before the file write finishes. A failed file read leaves the lines
// already noted.
func Recent() string {
	loadDiag()
	diagMu.Lock()
	defer diagMu.Unlock()
	return strings.Join(diagLines, "\n")
}

func loadDiag() {
	diagOnce.Do(func() {
		b, err := os.ReadFile(DiagPath())
		var lines []string
		if err == nil {
			text := strings.TrimRight(string(b), "\n")
			if text != "" {
				lines = strings.Split(text, "\n")
				if len(lines) > diagKeepLines {
					lines = lines[len(lines)-diagKeepLines:]
				}
			}
		}
		diagMu.Lock()
		diagLines = append(lines, diagLines...)
		if len(diagLines) > diagKeepLines {
			diagLines = append([]string(nil), diagLines[len(diagLines)-diagKeepLines:]...)
		}
		diagMu.Unlock()
	})
}

func ensureDiagWriter() {
	diagWriter.Do(func() {
		go func() {
			diagMu.Lock()
			for {
				for len(diagPending) == 0 {
					diagCond.Wait()
				}
				batch := diagPending
				diagPending = nil
				diagMu.Unlock()
				appendDiagBatch(batch)
				diagWG.Add(-len(batch))
				diagMu.Lock()
			}
		}()
	})
}

func sanitizeDiag(msg string) string {
	msg = strings.Join(strings.Fields(msg), " ")
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "private key") || strings.Contains(lower, "begin private") {
		return "redacted"
	}
	if i := strings.Index(lower, "bearer "); i >= 0 {
		msg = strings.TrimSpace(msg[:i] + "bearer [redacted]")
	}
	if len(msg) > 180 {
		msg = msg[:180]
	}
	return msg
}

func appendDiagBatch(batch []string) {
	if len(batch) == 0 {
		return
	}
	path := DiagPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	for _, line := range batch {
		_, _ = f.WriteString(line + "\n")
	}
	st, statErr := f.Stat()
	_ = f.Close()
	if statErr != nil || st.Size() <= diagMaxBytes {
		return
	}
	// The page buffer is already trimmed. Rewrite from it instead of
	// reading the file back on the path that just appended.
	diagMu.Lock()
	lines := append([]string(nil), diagLines...)
	diagMu.Unlock()
	body := strings.TrimRight(strings.Join(lines, "\n"), "\n")
	if body != "" {
		body += "\n"
	}
	_ = os.WriteFile(path, []byte(body), 0o600)
}
