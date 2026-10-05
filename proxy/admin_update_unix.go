//go:build !windows

package proxy

import "syscall"

// detachSysProcAttr puts the update script in its own session and process group
// so the service restart cannot take it down. launchctl kickstart -k (and
// systemd's stop) signal the job's process group; a child that called setsid is
// outside it and survives to finish the swap and the health check.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
