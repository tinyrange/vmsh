//go:build !windows

package vmshd

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestTerminateHostShellProcessSignalsDescendants(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 30 & echo ready; wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start process group: %v", err)
	}
	ready := make([]byte, 6)
	if _, err := stdout.Read(ready); err != nil {
		t.Fatalf("wait for child: %v", err)
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("get process group: %v", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	terminateHostShellProcess(cmd, exited, time.Second)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("shell process group still exists after termination")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
