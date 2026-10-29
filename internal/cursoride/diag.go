package cursoride

import (
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

// DiagPath is the log file. Installed, that is under the user's config
// folder (not Cursor's %APPDATA%\Cursor): ~/.config/magpie/cursor-ide
// on every system, $XDG_CONFIG_HOME/magpie/cursor-ide when that is set,
// or the data folder beside magpie when the run is portable.
func DiagPath() string {
	return filepath.Join(appdir.Config(), "cursor-ide", diagFileName)
}

// Note appends one timestamped line to the log and to the recent buffer
// the Agents page reads. Newlines are folded. A bearer token is not kept.
func Note(msg string) {
	msg = sanitizeDiag(msg)
	if msg == "" {
		return
	}
	line := time.Now().Format("2006-01-02 15:04:05") + " " + msg
	diagMu.Lock()
	diagLines = append(diagLines, line)
	if len(diagLines) > diagKeepLines {
		diagLines = append([]string(nil), diagLines[len(diagLines)-diagKeepLines:]...)
	}
	diagMu.Unlock()
	appendDiag(line)
}

// Recent is the log as the Agents page shows it: the file when it can be
// read, otherwise the lines still in memory. A failed read is not reported
// as an empty log when memory has lines.
func Recent() string {
	b, err := os.ReadFile(DiagPath())
	if err != nil {
		diagMu.Lock()
		defer diagMu.Unlock()
		return strings.Join(diagLines, "\n")
	}
	text := string(b)
	if len(text) > diagMaxBytes {
		text = text[len(text)-diagMaxBytes:]
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	return strings.TrimRight(text, "\n")
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

func appendDiag(line string) {
	path := DiagPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line + "\n")
	_ = f.Close()
	st, err := os.Stat(path)
	if err != nil || st.Size() <= diagMaxBytes {
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) > diagKeepLines {
		lines = lines[len(lines)-diagKeepLines:]
	}
	_ = os.WriteFile(path, []byte(strings.TrimRight(strings.Join(lines, "\n"), "\n")+"\n"), 0o600)
}
