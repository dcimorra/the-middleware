//go:build linux && (amd64 || arm64)

package sites

import (
	"os"
	"syscall"
	"unsafe"
)

// exchange swaps two folders in one step (renameat2 RENAME_EXCHANGE):
// at no moment is there no site folder, which two renames leave for the
// engine to find, as a site that was deleted, and stop.
func exchange(a, b string) error {
	pa, err := syscall.BytePtrFromString(a)
	if err != nil {
		return err
	}
	pb, err := syscall.BytePtrFromString(b)
	if err != nil {
		return err
	}
	atFDCWD := -100 // AT_FDCWD: the paths are read from the working directory
	const renameExchange = 2
	_, _, errno := syscall.Syscall6(sysRenameat2, uintptr(atFDCWD), uintptr(unsafe.Pointer(pa)), uintptr(atFDCWD), uintptr(unsafe.Pointer(pb)), renameExchange, 0)
	if errno == 0 {
		return nil
	}
	if errno == syscall.ENOSYS || errno == syscall.EINVAL {
		return exchangeByRenames(a, b)
	}
	return &os.LinkError{Op: "exchange", Old: a, New: b, Err: errno}
}
