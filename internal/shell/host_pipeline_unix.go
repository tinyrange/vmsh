//go:build darwin || linux

package shell

import (
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/tinyrange/vmsh/internal/terminal"
)

type hostCommandRunner interface {
	runHostCommand(cmd *exec.Cmd) error
}

type hostPipelineGroup struct {
	mu         sync.Mutex
	pgid       int
	started    <-chan struct{}
	foreground *terminal.ForegroundProcessGroup
}

func newHostPipelineGroup(stdin *os.File, started <-chan struct{}) *hostPipelineGroup {
	foreground, _ := terminal.NewForegroundProcessGroup(stdin)
	return &hostPipelineGroup{started: started, foreground: foreground}
}

func (g *hostPipelineGroup) runHostCommand(cmd *exec.Cmd) error {
	if g == nil {
		return cmd.Run()
	}
	g.mu.Lock()
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if g.pgid != 0 {
		cmd.SysProcAttr.Pgid = g.pgid
	}
	err := cmd.Start()
	if err == nil && g.pgid == 0 {
		g.pgid = cmd.Process.Pid
		if g.foreground != nil {
			_ = g.foreground.Set(g.pgid)
		}
	}
	g.mu.Unlock()
	if err != nil {
		return err
	}
	if g.started != nil {
		<-g.started
	}
	return cmd.Wait()
}

func (g *hostPipelineGroup) restore() {
	if g == nil || g.foreground == nil {
		return
	}
	_ = g.foreground.Restore()
}
