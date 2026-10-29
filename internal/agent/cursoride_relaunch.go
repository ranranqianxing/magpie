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

// errCursorIDENoRelaunch is a connect that did not quit Cursor: the test
// sandbox, or no ordinary Cursor install to open. Connect still succeeds.
var errCursorIDENoRelaunch = errors.New("cursor relaunch skipped")

// cursorIDERestarted is set when the connect that wrote settings also
// opened Cursor again. The notice reads it.
var cursorIDERestarted bool

// cursorIDEDoRelaunch quits Cursor and opens it again. Tests replace it
// so a connect never signals a Cursor on the machine that runs them.
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
	cursoride.Note("cursor relaunch opened")
	return nil
}

// cursorIDEAfterWrite runs only after Cursor's settings were written.
// A connect that already matches, KeepCursorIDE, and disconnect do not
// call it. A skipped or failed relaunch leaves the quit-and-reopen notice.
func cursorIDEAfterWrite() {
	err := cursorIDEDoRelaunch(cursorIDELook())
	cursorIDETrustMu.Lock()
	cursorIDERestarted = err == nil
	cursorIDETrustMu.Unlock()
	if err != nil && !errors.Is(err, errCursorIDENoRelaunch) {
		log.Printf("Cursor: relaunch: %v", err)
		cursoride.Note("cursor relaunch failed")
	}
}
