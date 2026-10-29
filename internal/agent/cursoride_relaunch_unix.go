//go:build !windows

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yetone/magpie/internal/proc"
)

func relaunchCursor(ctx context.Context, exe string) error {
	if runtime.GOOS == "darwin" {
		return relaunchCursorDarwin(ctx, exe)
	}
	return relaunchCursorLinux(ctx, exe)
}

func cursorBundle(exe string) string {
	dir := exe
	for i := 0; i < 6; i++ {
		if strings.HasSuffix(dir, ".app") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func relaunchCursorDarwin(ctx context.Context, exe string) error {
	bundle := cursorBundle(exe)
	if bundle == "" {
		return errors.New("cursor bundle")
	}
	pat := regexp.QuoteMeta(bundle + "/Contents/")
	if len(running(pat)) == 0 {
		return appOpen(ctx, bundle)
	}
	if err := appQuit(ctx, bundle); err != nil {
		return err
	}
	end := time.Now().Add(appCloseWait)
	for len(running(pat)) > 0 {
		if time.Now().After(end) {
			return errors.New("Cursor is still open")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(appPoll):
		}
	}
	return appOpen(ctx, bundle)
}

func relaunchCursorLinux(ctx context.Context, exe string) error {
	pids := pidsOfExe(exe)
	if len(pids) > 0 {
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
		end := time.Now().Add(appCloseWait)
		for len(pidsOfExe(exe)) > 0 {
			if !time.Now().Before(end) {
				for _, pid := range pidsOfExe(exe) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
				break
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(appPoll):
			}
		}
	}
	cmd := proc.Command(exe)
	cmd.Dir = filepath.Dir(exe)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func pidsOfExe(exe string) []int {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	want := filepath.Clean(exe)
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		want = filepath.Clean(real)
	}
	var pids []int
	self := os.Getpid()
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		link, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe"))
		if err != nil {
			continue
		}
		link = strings.TrimSuffix(link, " (deleted)")
		if filepath.Clean(link) == want || filepath.Clean(link) == filepath.Clean(exe) {
			pids = append(pids, pid)
		}
	}
	return pids
}
