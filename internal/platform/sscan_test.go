//go:build !windows

package platform

import "fmt"

func sscan(s string, v *int) (int, error) { return fmt.Sscan(s, v) }
