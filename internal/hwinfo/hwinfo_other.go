//go:build !windows && !linux && !darwin

package hwinfo

// Other platforms report only the logical CPU count, which Detect fills in.
func detect() Specs { return Specs{} }
