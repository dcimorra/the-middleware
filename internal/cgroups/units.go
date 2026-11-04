package cgroups

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseBytes reads a memory size the way docker's --memory does ("512m",
// "1g", "1.5GiB", "2048"): a number, an optional space, and k, m, g, t or
// p with an optional i and b, all binary multiples, case does not matter.
// One parser for docker.memory, a site's limits and the settings, so the
// same "1g" never means two sizes in two places of the same manifest.
func ParseBytes(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("empty size")
	}
	low := strings.ToLower(t)
	low = strings.TrimSuffix(low, "b")
	low = strings.TrimSuffix(low, "i")
	mult := int64(1)
	if n := len(low); n > 0 {
		switch low[n-1] {
		case 'k':
			mult = 1 << 10
		case 'm':
			mult = 1 << 20
		case 'g':
			mult = 1 << 30
		case 't':
			mult = 1 << 40
		case 'p':
			mult = 1 << 50
		}
		if mult > 1 {
			low = low[:n-1]
		}
	}
	low = strings.TrimSpace(low)
	f, err := strconv.ParseFloat(low, 64)
	if err != nil || f < 0 || low == "" || strings.ContainsAny(low, "eE+-xXnN") {
		return 0, fmt.Errorf("%q is not a size (512m, 1g, 1.5g)", s)
	}
	v := f * float64(mult)
	if v >= 1<<62 {
		return 0, fmt.Errorf("%q is too large", s)
	}
	return int64(v), nil
}
