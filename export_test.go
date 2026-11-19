package s4ql

import (
	"context"
	"database/sql"
	"database/sql/driver"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/store"
)

// OpenStore is [Open] on the store s and the host h, with the options co of the storage core. Of
// the options o it uses what does not name the store, and the endpoint, the bucket, and the prefix
// for the identity of the database, in place of co.Identity.
func OpenStore(ctx context.Context, h host.Host, s store.Store, o Options, co core.Options) (*sql.DB, error) {
	d := storeOpener(h, s, o, co)
	c, err := d.connect(ctx, d)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(c), nil
}

// RegisterStore is [Register] on the store s and the host h, with the options co of the storage
// core.
func RegisterStore(name string, h host.Host, s store.Store, o Options, co core.Options) {
	sql.Register(name, storeOpener(h, s, o, co))
}

func storeOpener(h host.Host, s store.Store, o Options, co core.Options) *opener {
	return &opener{connect: func(ctx context.Context, d driver.Driver) (*connector, error) {
		return newConnector(ctx, h, s, o, co, d)
	}}
}
