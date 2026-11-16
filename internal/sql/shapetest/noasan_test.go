//go:build !asan

package shapetest

// asanEnabled reports whether the test binary is built with the address sanitizer.
const asanEnabled = false
