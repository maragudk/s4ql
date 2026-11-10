/*
** A test program, built by the tests with options.h and the vendored SQLite, that shows the effect of
** SQLITE_STRICT_SUBTYPE. It registers one function twice, once declaring that it sets a subtype with
** SQLITE_RESULT_SUBTYPE and once without, and prints the outcome of calling each. The binding cannot
** register functions, so this effect is visible only from C.
*/
#include <stdio.h>

#include "sqlite3.h"

static void subtype(sqlite3_context *ctx, int argc, sqlite3_value **argv) {
	(void)argc;
	(void)argv;
	sqlite3_result_int(ctx, 1);
	sqlite3_result_subtype(ctx, 74);
}

int main(void) {
	sqlite3 *db;
	if (sqlite3_open(":memory:", &db) != SQLITE_OK) {
		fprintf(stderr, "open: %s\n", sqlite3_errmsg(db));
		return 1;
	}
	if (sqlite3_create_function(db, "declared", 0, SQLITE_UTF8 | SQLITE_RESULT_SUBTYPE, 0, subtype, 0, 0) != SQLITE_OK ||
		sqlite3_create_function(db, "undeclared", 0, SQLITE_UTF8, 0, subtype, 0, 0) != SQLITE_OK) {
		fprintf(stderr, "create function: %s\n", sqlite3_errmsg(db));
		return 1;
	}

	const char *names[] = {"declared", "undeclared"};
	for (int i = 0; i < 2; i++) {
		char sql[64];
		snprintf(sql, sizeof(sql), "select %s()", names[i]);
		sqlite3_stmt *stmt;
		if (sqlite3_prepare_v2(db, sql, -1, &stmt, 0) != SQLITE_OK) {
			fprintf(stderr, "prepare: %s\n", sqlite3_errmsg(db));
			return 1;
		}
		int rc = sqlite3_step(stmt);
		if (rc == SQLITE_ROW) {
			printf("%s: ok\n", names[i]);
		} else {
			printf("%s: error %d: %s\n", names[i], rc, sqlite3_errmsg(db));
		}
		sqlite3_finalize(stmt);
	}
	return sqlite3_close(db) == SQLITE_OK ? 0 : 1;
}
