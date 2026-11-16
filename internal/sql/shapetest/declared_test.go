package shapetest_test

import (
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/shapetest"
)

// TestCheck_declared checks statements in which what the tree declares follows one of SQLite's rules
// for what it records of a table, an index, or a view, and requires that the check compared it.
func TestCheck_declared(t *testing.T) {
	var c shapetest.Checker
	defer c.Close()
	for _, sql := range shapetest.Declared {
		t.Run(sql, func(t *testing.T) {
			r := c.Check(sql)
			is.Equal(t, "", r.Fault)
			is.True(t, r.Declared, "not compared: "+r.Way.String()+" "+r.Reason)
		})
	}
}
