//go:build !unix

package locals3

import "syscall"

func detachedAttr() *syscall.SysProcAttr { return nil }
