//go:build !linux || !(amd64 || arm64)

package sites

// exchange is two renames where the kernel cannot swap two folders.
func exchange(a, b string) error { return exchangeByRenames(a, b) }
