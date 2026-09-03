//go:build !windows

package main

import "syscall"

// reexecUnix replaces the current process image with the binary at path (Unix only), so the
// freshly-updated binary handles this same invocation. Never returns on success.
func reexecUnix(path string, args, env []string) error {
	return syscall.Exec(path, args, env)
}
