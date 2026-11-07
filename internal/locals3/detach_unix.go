//go:build unix

package locals3

import "syscall"

// detachedAttr starts the server in its own session so Ctrl-C or a closed
// terminal does not stop it.
func detachedAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
