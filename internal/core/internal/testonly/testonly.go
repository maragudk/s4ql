// Package testonly has the key that the hooks of the storage core for tests take, such as
// [maragu.dev/s4ql/internal/core.SyncOff]. Go lets only the packages under internal/core import this
// package, so no other package can make a key and call such a hook.
package testonly

// Key that a hook of the storage core for tests takes.
type Key struct{}
