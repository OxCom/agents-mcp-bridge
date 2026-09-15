package run

import "fmt"

func sscanInt(s string, v *int) (int, error) { return fmt.Sscan(s, v) }
