//go:build !darwin && !linux && !windows

package shell

import "sync"

var sshKnownHostsProcessLock sync.Mutex

func withSSHKnownHostsLock(_ string, fn func() error) error {
	sshKnownHostsProcessLock.Lock()
	defer sshKnownHostsProcessLock.Unlock()
	return fn()
}
