//go:build !windows

package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/proc"
)

func TestRelaunchCursorStopsTheRunningBinaryAndStartsIt(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("this checks the Linux /proc match")
	}
	src, err := os.ReadFile("/bin/sleep")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "cursor-bin")
	if err := os.WriteFile(bin, src, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := proc.Command(bin, "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for !pidListed(pidsOfExe(bin), cmd.Process.Pid) {
		if time.Now().After(deadline) {
			t.Fatal("the binary never showed up in /proc")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := relaunchCursor(context.Background(), bin); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("the running binary was not stopped")
	}
}

func pidListed(pids []int, pid int) bool {
	for _, p := range pids {
		if p == pid {
			return true
		}
	}
	return false
}
