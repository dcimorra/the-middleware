//go:build linux && (amd64 || arm64)

package cluster

import (
	"syscall"
	"unsafe"
)

// renameat2 with RENAME_EXCHANGE swaps two paths in one step. Two renames
// leave a moment with no folder at the site's name, and an engine tick in
// that moment stops the replica: a request the primary sends then finds
// nothing. syscall has no wrapper, and amd64 not even the number.
const renameExchange = 2

func exchange(a, b string) error {
	pa, err := syscall.BytePtrFromString(a)
	if err != nil {
		return err
	}
	pb, err := syscall.BytePtrFromString(b)
	if err != nil {
		return err
	}
	const atFDCWD = -100
	fd := atFDCWD
	_, _, errno := syscall.Syscall6(sysRenameat2, uintptr(fd), uintptr(unsafe.Pointer(pa)), uintptr(fd), uintptr(unsafe.Pointer(pb)), renameExchange, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
