// Package drivercheck holds the differential test of the database/sql driver of s4ql against
// github.com/mattn/go-sqlite3: the same texts run through the [database/sql.DB] of each, and the
// test compares what an application sees, the results, the rows and their Go types, the columns,
// the errors, and which call fails.
//
// mattn's SQLite is the oracle: the tests of the module above require it to be the C code of the
// SQLite that s4ql pins, byte for byte, so that its messages are the pinned SQLite's. It is a
// driver that s4ql did not write, so it judges what s4ql's driver adds with another reading of
// database/sql than s4ql's own. s4ql is opened with [maragu.dev/s4ql.Open], as an application
// opens it, on a fake S3 server in memory, so the test runs the real store client, core, engine,
// and cache, and needs no seam in s4ql's code.
package drivercheck
