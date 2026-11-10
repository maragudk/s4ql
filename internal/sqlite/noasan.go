//go:build !asan

package sqlite

// asan is true when the binary is built with the address sanitizer, by go build -asan.
const asan = false
