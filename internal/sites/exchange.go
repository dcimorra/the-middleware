package sites

import "os"

// exchangeByRenames swaps a and b with a third name, for a kernel or a
// file system without RENAME_EXCHANGE. For the moment between the two
// renames b does not exist.
func exchangeByRenames(a, b string) error {
	tmp := b + ".exchanging"
	if err := os.Rename(b, tmp); err != nil {
		return err
	}
	if err := os.Rename(a, b); err != nil {
		_ = os.Rename(tmp, b)
		return err
	}
	return os.Rename(tmp, a)
}
