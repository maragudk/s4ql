// Package drivercheck holds the differential test of the database/sql driver of s4ql against
// github.com/mattn/go-sqlite3: the same texts run through the [database/sql.DB] of each, and the
// test compares what an application sees, the results, the rows and their Go types, the columns,
// the errors, and which call fails. The texts run with arguments too, directly and through prepared
// statements that run several times. A call that s4ql refuses for its arguments by its own rule,
// another number of arguments than the statement has parameters or an argument that s4ql does not
// support, is checked against that rule and not run in mattn's SQLite, and the test pins how many
// such calls there are.
//
// mattn's SQLite is the oracle: the tests of the module above require it to be the C code of the
// SQLite that s4ql pins, byte for byte, so that its messages are the pinned SQLite's. It is a
// driver that s4ql did not write, so it judges what s4ql's driver adds with another reading of
// database/sql than s4ql's own. s4ql is opened with [maragu.dev/s4ql.Open], as an application
// opens it, on a fake S3 server in memory, so the test runs the real store client, core, engine,
// and cache, and needs no seam in s4ql's code.
package drivercheck
