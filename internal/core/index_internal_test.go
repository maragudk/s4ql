package core

import (
	"context"

	"maragu.dev/s4ql/internal/sqlite"
)

// ScanPlan is the statement that [ReadTx.ScanIndex] runs for the scan, and SQLite's plan for it on
// the connection of the transaction: the details of the rows of "explain query plan", in their
// order. It fails as ScanIndex does.
func (r *ReadTx) ScanPlan(ctx context.Context, name string, scan IndexScan) (string, []string, error) {
	const operation = "scan"
	if err := r.check(); err != nil {
		return "", nil, err
	}
	defer r.leave()
	t, err := r.table(ctx, operation, name)
	if err != nil {
		return "", nil, err
	}
	ix, err := scanned(t, scan, r.c.limit)
	if err != nil {
		return "", nil, err
	}
	sql, _ := t.scanSQL(ix, scan)
	var plan []string
	err = r.c.query("explain query plan "+sql, func(s *sqlite.Stmt) error {
		plan = append(plan, s.ColumnText(3))
		return nil
	})
	return sql, plan, r.d.cacheError(ctx, operation, err)
}
