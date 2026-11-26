// Package testonly has the key that the hooks of the storage core for tests take. Go lets only the
// packages under internal/core import this package, and the key has an unexported field, so that
// no other package can make one, not even as a literal of the same struct type.
package testonly

// Key that a hook of the storage core for tests takes.
type Key struct{ _ struct{} }
