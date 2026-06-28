//go:build !darwin && !linux

package shell

import (
	"os"
	"os/exec"
)

type hostCommandRunner interface {
	runHostCommand(cmd *exec.Cmd) error
}

type hostPipelineGroup struct{}

func newHostPipelineGroup(*os.File, <-chan struct{}) *hostPipelineGroup {
	return &hostPipelineGroup{}
}

func (g *hostPipelineGroup) runHostCommand(cmd *exec.Cmd) error {
	return cmd.Run()
}

func (g *hostPipelineGroup) restore() {}
