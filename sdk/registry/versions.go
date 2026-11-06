package registry

import (
	"fmt"
	"strconv"
	"time"
)

// VersionDepth selects current-only, up to N versions, all published versions,
// or versions created on/after an inclusive UTC history cutoff.
type VersionDepth struct {
	Max   int
	Since time.Time
}

func ParseVersionDepth(spec string) (VersionDepth, error) {
	if spec == "" {
		spec = "1"
	}
	if spec == "unlimited" {
		return VersionDepth{}, nil
	}
	if date, err := time.Parse(time.DateOnly, spec); err == nil {
		return VersionDepth{Since: date}, nil
	}
	n, err := strconv.Atoi(spec)
	if err != nil || n < 1 {
		return VersionDepth{}, fmt.Errorf("version depth must be a positive integer, YYYY-MM-DD history cutoff, or unlimited")
	}
	return VersionDepth{Max: n}, nil
}
