//go:build windows

package main

import "errors"

// reexecUnix is unavailable on Windows; the Windows path in firstLaunchUpdateAndReexec uses
// exec.Command instead, so this should never be called. Provided to satisfy the compiler.
func reexecUnix(path string, args, env []string) error {
	return errors.New("reexecUnix not supported on windows")
}
