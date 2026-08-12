package ossutil

// NormalizeListLimit applies the provider-neutral default and maximum page
// sizes.
func NormalizeListLimit(limit int) int {
	switch {
	case limit <= 0:
		return 100
	case limit > 1000:
		return 1000
	default:
		return limit
	}
}
