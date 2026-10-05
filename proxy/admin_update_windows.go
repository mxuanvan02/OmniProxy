//go:build windows

package proxy

import "syscall"

// Windows has no setsid, and the release builds a windows/amd64 binary, so this
// must exist for the build to compile. The update script requires an explicit
// OMNIPROXY_RESTART_CMD on Windows anyway (see its header), and the release
// itself does not ship scripts/, so a Windows binary reports "cannot
// self-update" rather than pretending to.
func detachSysProcAttr() *syscall.SysProcAttr {
	return nil
}
