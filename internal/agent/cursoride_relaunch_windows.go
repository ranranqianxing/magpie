//go:build windows

package agent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"
)

func relaunchCursor(ctx context.Context, exe string) error {
	ps := cursorProcs(exe)
	if len(ps) == 0 {
		return winLaunch(winProc{image: exe})
	}
	pids := make([]int, len(ps))
	main := ps[0]
	mine := map[int]bool{}
	for i, p := range ps {
		pids[i] = p.pid
		mine[p.pid] = true
	}
	for _, p := range ps {
		if !mine[p.parent] {
			main = p
			break
		}
	}
	_ = winClose(pids)
	if !waitCursorGone(ctx, exe, appCloseWait) {
		if err := winEnd(main.pid); err != nil || !waitCursorGone(ctx, exe, appCloseWait) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("Cursor is still running")
		}
	}
	return winLaunch(winProc{image: exe})
}

func cursorProcs(exe string) []winProc {
	var ps []winProc
	for _, p := range winProcs(filepath.Base(exe)) {
		if strings.EqualFold(filepath.Clean(p.image), filepath.Clean(exe)) {
			ps = append(ps, p)
		}
	}
	return ps
}

func waitCursorGone(ctx context.Context, exe string, limit time.Duration) bool {
	end := time.Now().Add(limit)
	for {
		if len(cursorProcs(exe)) == 0 {
			return true
		}
		if !time.Now().Before(end) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(appPoll):
		}
	}
}
