package agent

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/cursoride"
	"github.com/yetone/magpie/internal/proc"
)

// errCursorIDENoRelaunch is a connect or disconnect that did not quit
// Cursor: the test sandbox, or no ordinary Cursor install to open. The
// settings change still succeeds.
var errCursorIDENoRelaunch = errors.New("cursor relaunch skipped")

// cursorIDERestarted is set when the connect or disconnect that wrote
// settings also opened Cursor again. The notice reads it.
// cursorIDERestored is set when that write was a disconnect putting the
// previous settings back.
var (
	cursorIDERestarted bool
	cursorIDERestored  bool
)

// cursorIDEDoRelaunch quits Cursor and opens it again. Tests replace it
// so a connect or disconnect never signals a Cursor on the machine that
// runs them.
var cursorIDEDoRelaunch = func(exe string) error {
	if proc.Isolated() {
		return errCursorIDENoRelaunch
	}
	if strings.TrimSpace(exe) == "" {
		cursoride.Note("cursor relaunch skipped: Cursor was not found")
		return errCursorIDENoRelaunch
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := relaunchCursor(ctx, exe); err != nil {
		return err
	}
	return nil
}

// cursorIDEAfterWrite runs only after Cursor's settings were written or
// restored. A connect that already matches, KeepCursorIDE, and a
// disconnect that leaves the file alone do not call it. A skipped or
// failed relaunch leaves the quit-and-reopen notice. The diagnostics
// line is the same one a connect writes.
func cursorIDEAfterWrite() {
	err := cursorIDEDoRelaunch(cursorIDELook())
	cursorIDETrustMu.Lock()
	cursorIDERestarted = err == nil
	cursorIDETrustMu.Unlock()
	switch {
	case err == nil:
		cursoride.Note("cursor relaunch opened")
	case !errors.Is(err, errCursorIDENoRelaunch):
		log.Printf("Cursor: relaunch: %v", err)
		cursoride.Note("cursor relaunch failed")
	}
}
