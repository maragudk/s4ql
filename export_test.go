package s4ql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/store"
)

// MaxOpenConns is [maxOpenConns].
var MaxOpenConns = maxOpenConns

// FileLimit is [fileLimit].
var FileLimit = fileLimit

// PublicError is [publicError].
var PublicError = publicError

// Value is [value].
var Value = value

// OpenStore is [Open] on the store s and the host h, with the options co of the storage core. Of
// the options o it uses what does not name the store, and the endpoint, the bucket, and the prefix
// for the identity of the database, in place of co.Identity.
func OpenStore(ctx context.Context, h host.Host, s store.Store, o Options, co core.Options) (*sql.DB, error) {
	return storeOpener(h, s, o, co).openDB(ctx)
}

// OpenOwnedStore is [OpenStore] on a store that the instance owns, and closes, as it does the store
// that [Open] makes.
func OpenOwnedStore(ctx context.Context, h host.Host, s interface {
	store.Store
	io.Closer
}, o Options, co core.Options) (*sql.DB, error) {
	return (&opener{connect: func(ctx context.Context, d driver.Driver, maxOpen int) (*connector, error) {
		return ownedConnector(ctx, h, s, o, co, d, maxOpen)
	}}).openDB(ctx)
}

// RegisterStore is [Register] on the store s and the host h, with the options co of the storage
// core.
func RegisterStore(name string, h host.Host, s store.Store, o Options, co core.Options) {
	sql.Register(name, storeOpener(h, s, o, co))
}

func storeOpener(h host.Host, s store.Store, o Options, co core.Options) *opener {
	return &opener{connect: func(ctx context.Context, d driver.Driver, maxOpen int) (*connector, error) {
		return newConnector(ctx, h, s, o, co, d, maxOpen)
	}}
}
