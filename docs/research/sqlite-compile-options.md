# SQLite 3.54.1 compile-time options: classification for s4ql

Date: 2026-11-10.

Question: which of SQLite's compile-time options should the build in `internal/sqlite` set? Every one of the 214 options on compile.html is classified below.

Outcome: Markus agreed ten options on 2026-11-10, as a default that can change later. They are the seven that were already set (`SQLITE_DQS=0`, `SQLITE_DEFAULT_FOREIGN_KEYS=1`, `SQLITE_THREADSAFE=1`, `SQLITE_ENABLE_API_ARMOR`, `SQLITE_OMIT_AUTORESET`, `SQLITE_OMIT_SHARED_CACHE`, `SQLITE_OMIT_LOAD_EXTENSION`) plus `SQLITE_MAX_MMAP_SIZE=0`, `SQLITE_ENABLE_MATH_FUNCTIONS`, and `SQLITE_STRICT_SUBTYPE=1`. The agreed set departs from the recommendations below in two places: `SQLITE_OMIT_DEPRECATED`, marked SET, was left out, and there is no test-only build, so `SQLITE_DEBUG`, marked TEST, is not used. [../../internal/sqlite/options.h](../../internal/sqlite/options.h) is the source of truth for what is set, and it gives the reasons, also for the options left out.

Source: https://sqlite.org/compile.html (downloaded 2026-11-10, "last updated on 2026-10-01"). References of the form `#anchor` are anchors on that page. Other pages: mmap.html, wal.html, security.html, quirks.html, howtocompile.html, cli.html, limits.html, c3ref/c_dbconfig_defensive.html.

Marks: **[V]** read in the docs; **[T]** tested in this run against the vendored amalgamation (builds not published); **[S]** read in the vendored `sqlite3.c`; **[I]** inferred.

Recommendation codes:

- **KEEP**: already in `options.h`, keep it.
- **SET**: add to the default build now.
- **LATER**: add when a slice needs it.
- **TEST**: only for a test-only build variant.
- **DEFAULT**: do not define it. The default is stated where it matters.
- **OUT**: do not define it, for a stated reason. Stronger than DEFAULT: the option is tempting or is on a recommended list.
- **N/A**: does not apply to this platform or build.
- **RUNTIME**: do not define it. Pin the same setting per connection at run time instead.

Count: compile.html has 203 option entries in sections 3 to 12 [T: headings extracted from the page], plus 10 size-limit macros listed in section 5 and documented on limits.html, plus `SQLITE_CUSTOM_INCLUDE` in section 2. That makes 214 names, and every one is classified below.

## 2. Recommended compile-time options (#rcmd)

compile.html says this set "is one configuration that is always fully tested". It is recommended "in order to minimize the number of CPU cycles and the bytes of memory used". The reason is performance, not correctness [V]. Each item is classified in its own section below. Summary:

| # | Option | s4ql |
|---|---|---|
| 1 | SQLITE_DQS=0 | KEEP |
| 2 | SQLITE_THREADSAFE=0 | OUT (keep =1) |
| 3 | SQLITE_DEFAULT_MEMSTATUS=0 | OUT |
| 4 | SQLITE_DEFAULT_WAL_SYNCHRONOUS=1 | OUT (decide at run time) |
| 5 | SQLITE_LIKE_DOESNT_MATCH_BLOBS | OUT (changes results) |
| 6 | SQLITE_MAX_EXPR_DEPTH=0 | OUT (removes an error) |
| 7 | SQLITE_OMIT_DECLTYPE | OUT |
| 8 | SQLITE_OMIT_DEPRECATED | SET |
| 9 | SQLITE_OMIT_PROGRESS_CALLBACK | OUT |
| 10 | SQLITE_OMIT_SHARED_CACHE | KEEP |
| 11 | SQLITE_OMIT_AUTOINIT | OUT |
| 12 | SQLITE_STRICT_SUBTYPE=1 | SET |

| Option | Rec | Reason |
|---|---|---|
| SQLITE_CUSTOM_INCLUDE | N/A | "will cause sqlite3.c to include myconfig.h early on" [V]. The binding and the shell already include `options.h` first (`#include` and `-include`), which has the same effect [S]. |

## 3. Platform configuration (#osconfig)

| Option | Rec | Reason |
|---|---|---|
| _HAVE_SQLITE_CONFIG_H | N/A | For configure-generated "HAVE_INTERFACE" options, "not library-level configuration" [V]. |
| HAVE_FDATASYNC | DEFAULT (fsync) | Selects fdatasync() over fsync(). Without it, "fsync() is always used" [V]. fsync is the more conservative choice for the cache [I]. |
| HAVE_GMTIME_R | DEFAULT | Applies only together with SQLITE_OMIT_DATETIME_FUNCS [V]. |
| HAVE_ISNAN | DEFAULT | Without it, SQLite "substitutes its own home-grown implementation of isnan()" [V]. The built-in version is the same on every platform [I]. |
| HAVE_LOCALTIME_R | DEFAULT | Thread-safe localtime() for the `localtime` modifier [V]. The `localtime` modifier depends on the process time zone, so the engine should not support it without a decision [I]. Revisit if it is ever supported. |
| HAVE_LOCALTIME_S | N/A | Windows. |
| HAVE_MALLOC_USABLE_SIZE | DEFAULT | Memory accounting only. "Applications are encouraged to use HAVE_MALLOC_USABLE_SIZE whenever possible" [V], but it does not apply on Apple ("zone malloc") [V], and it would make Linux and macOS builds differ [I]. |
| HAVE_STRCHRNUL | DEFAULT | Libc substitution only [V]. |
| HAVE_UTIME | N/A | Affects only the "unix-dotfile" VFS [V]. |
| SQLITE_BYTEORDER | DEFAULT | "#ifdefs in the code that set SQLITE_BYTEORDER automatically for all common platforms" [V]. |

## 4. Default parameter values (#defaults)

| Option | Rec | Reason |
|---|---|---|
| SQLITE_DEFAULT_AUTOMATIC_INDEX | DEFAULT (on) | Affects query plans only [V]. |
| SQLITE_DEFAULT_AUTOVACUUM | DEFAULT (0, off) | "The default value is 0" [V]. |
| SQLITE_DEFAULT_CACHE_SIZE | DEFAULT (-2000) | Performance only [V]. |
| SQLITE_DEFAULT_FILE_FORMAT | DEFAULT (4) | Format 4 "understands descending indices" [V]. Format 1 is legacy. |
| SQLITE_DEFAULT_FILE_PERMISSIONS | DEFAULT (0644) | Not a correctness matter. Set permissions when the cache is created if they matter [I]. |
| SQLITE_DEFAULT_FOREIGN_KEYS=1 | KEEP | Roadmap: "Foreign keys are always enforced." The docs add: "Each database connection can always turn enforcement of foreign key constraints on and off at run-time" [V]. Assert the setting when a connection opens [I]. Stock builds default to off [V], so the oracle intentionally differs from stock. |
| SQLITE_DEFAULT_MMAP_SIZE | DEFAULT (0) | "If N is zero, then memory mapped I/O is disabled by default" [V]. Has no effect once SQLITE_MAX_MMAP_SIZE=0 is set. |
| SQLITE_DEFAULT_JOURNAL_SIZE_LIMIT | DEFAULT (-1, no limit) | Disk usage only [V]. |
| SQLITE_DEFAULT_LOCKING_MODE | DEFAULT (0, NORMAL) | EXCLUSIVE mode would let WAL work without the mmapped -shm file (wal.html, "as long as the locking_mode is set to EXCLUSIVE") [V]. But it would also block the parallel read connections that the spec requires [I]. |
| SQLITE_DEFAULT_LOOKASIDE | DEFAULT | Performance. The binding already turns lookaside off at run time under -asan (README) [V]. |
| SQLITE_DEFAULT_MEMSTATUS=0 | OUT | On the recommended list, for speed. But "SQLITE_MAX_MEMORY ... non-functional when memory usage tracking is disabled", and so are sqlite3_memory_used() and sqlite3_memory_highwater() [V]. That would remove the memory-limit tools that security.html recommends [V]. |
| SQLITE_DEFAULT_PCACHE_INITSZ | DEFAULT | Performance [V]. |
| SQLITE_DEFAULT_PAGE_SIZE | DEFAULT (4096) | [V] |
| SQLITE_DEFAULT_SYNCHRONOUS | DEFAULT (2, FULL) | Durability setting. If a different value is wanted, choose it per connection at run time [I]. |
| SQLITE_DEFAULT_WAL_SYNCHRONOUS=1 | OUT (decide at run time) | On the recommended list. With NORMAL in WAL mode, "recent changes to the database might be rolled back by a power loss, but the database will not be corrupted" [V]. The cache would then be behind S3 and catch up, never ahead [I]. That is a performance trade-off, so decide it explicitly with PRAGMA synchronous [I]. Apple's /usr/bin/sqlite3 and mattn compile it in [T]. |
| SQLITE_DEFAULT_WAL_AUTOCHECKPOINT | DEFAULT (1000) | Performance [V]. |
| SQLITE_DEFAULT_WORKER_THREADS | DEFAULT (0) | [V]. Extra sorter threads do not change results but add scheduling variation [I]. |
| SQLITE_DQS=0 | KEEP | Roadmap restriction. Recommended: "The recommended setting is 0 ... the default setting is 3 for maximum compatibility" [V]. quirks.html: "Application developers are encouraged to compile using -DSQLITE_DQS=0" [V], and since 3.41.0 the CLI turns DQS off by default [V]. The `DEFAULT "abc"` exception remains (documented in options.h and the inventory note). |
| SQLITE_EXTRA_DURABLE | N/A | "This option is no longer supported" [V]. |
| SQLITE_FTS3_MAX_EXPR_DEPTH | N/A | FTS3/4 only [V]. |
| SQLITE_JSON_MAX_DEPTH | DEFAULT (1000) | Changing it changes which JSON inputs fail, compared with stock [I]. |
| SQLITE_LIKE_DOESNT_MATCH_BLOBS | OUT | On the recommended list, but it changes results: "causes the LIKE operator to always return False if either operand is a BLOB. The default behavior of LIKE is that BLOB operands are cast to TEXT" and "at the expense of breaking backwards compatibility" [V]. An oracle must not do this. Measured on the default build: `x'61' like 'a'` = 1 [T, UTF-8 database]. |
| SQLITE_MAX_MEMORY | DEFAULT (none) | A hard limit could be set at run time with sqlite3_hard_heap_limit64() if wanted (security.html) [V]. |
| SQLITE_MAX_MMAP_SIZE=0 | SET | "Setting this value to 0 completely disables memory-mapped I/O and causes logic associated with memory-mapped I/O to be omitted from the build" [V]. mmap.html: "An I/O error on a memory-mapped file cannot be caught and dealt with by SQLite. Instead, the I/O error causes a signal which, if not caught by the application, results in a program crash" [V]. The default build caps at 0x7fff0000, so `PRAGMA mmap_size=268435456` returns 268435456 (turned on at run time). With =0 it returns 0 [T]. **Caveat:** this covers the database file only. In WAL mode, "The wal-index is implemented using an ordinary file that is mmapped" (wal.html) [V]. The unix VFS keeps the mmap() syscall whenever WAL is compiled in (`#if (!defined(SQLITE_OMIT_WAL) \|\| SQLITE_MAX_MMAP_SIZE>0)`) [S]. A WAL database still worked with =0 [T]. |
| SQLITE_MAX_SCHEMA_RETRY | DEFAULT (50) | [V] |
| SQLITE_MAX_WORKER_THREADS | DEFAULT (8) | [V] |
| SQLITE_MEMDB_DEFAULT_MAXSIZE | DEFAULT (1 GiB) | Applies to sqlite3_deserialize databases only [V]. |
| SQLITE_MINIMUM_FILE_DESCRIPTOR | DEFAULT (3) | "a defense against accidental database corruption" [V]. Never set it to 0. |
| SQLITE_POWERSAFE_OVERWRITE | DEFAULT | Changes the VFS's assumption about writes during a power loss [V]. The page does not state the default. |
| SQLITE_PRINTF_PRECISION_LIMIT | OUT | security.html suggests it, but "No error is raised if a width or precision exceeds SQLITE_PRINTF_PRECISION_LIMIT. Instead, the large width or precision is silently truncated" [V]. That changes printf() results silently. |
| SQLITE_QUERY_PLANNER_LIMIT | DEFAULT | Plans only. "set high enough so that is never reached for real-world queries" [V]. |
| SQLITE_QUERY_PLANNER_LIMIT_INCR | DEFAULT | Same as above [V]. |
| SQLITE_REVERSE_UNORDERED_SELECTS | OUT (RUNTIME, test) | "useful for detecting when applications (incorrectly) assume that the order of rows" [V]. Valuable, but the harness should switch it per connection with PRAGMA reverse_unordered_selects / SQLITE_DBCONFIG_REVERSE_SCANORDER, so that both orders get exercised [I]. Compiling it in would make every connection, the cache included, differ from stock. |
| SQLITE_SORTER_PMASZ | DEFAULT | Performance [V]. |
| SQLITE_STMTJRNL_SPILL | DEFAULT | Performance [V]. |
| SQLITE_WIN32_MALLOC | N/A | Windows. |
| YYSTACKDEPTH | DEFAULT (100) | A different value changes which statements parse [I]. |

## 5. Size limits (#limits, limits.html)

The defaults reported by the vendored build [T]: MAX_ATTACHED=10, MAX_COLUMN=2000, MAX_COMPOUND_SELECT=500, MAX_EXPR_DEPTH=1000, MAX_FUNCTION_ARG=1000, MAX_LENGTH=1000000000, MAX_LIKE_PATTERN_LENGTH=50000, MAX_PAGE_COUNT=0xfffffffe, MAX_SQL_LENGTH=1000000000, MAX_VARIABLE_NUMBER=32766. Other builds compile in other values: Homebrew uses MAX_VARIABLE_NUMBER=250000 and Apple uses 500000 [T/V-measured in the inventory note].

| Option | Rec | Reason |
|---|---|---|
| SQLITE_MAX_ATTACHED | DEFAULT; RUNTIME | Lower it with sqlite3_limit() if the engine forbids ATTACH (security.html suggests 0) [V]. |
| SQLITE_MAX_COLUMN | DEFAULT | Limits "set a hard upper bound that can be changed at run-time ... using the sqlite3_limit() interface" [V]. |
| SQLITE_MAX_COMPOUND_SELECT | DEFAULT; RUNTIME | [V] |
| SQLITE_MAX_EXPR_DEPTH | DEFAULT (1000); OUT for =0 | `=0` "disables all checking of the expression parse-tree depth" [V]. That removes the error stock SQLite raises, and the depth check is what protects the stack from deep inputs (security.html, "deep recursion and hence a CPU stack overflow") [V]. Fuzzing makes such inputs likely [I]. |
| SQLITE_MAX_FUNCTION_ARG | DEFAULT; RUNTIME | [V] |
| SQLITE_MAX_LENGTH | DEFAULT | The cache must hold every value the engine accepts [I]. |
| SQLITE_MAX_LIKE_PATTERN_LENGTH | DEFAULT; RUNTIME | [V] |
| SQLITE_MAX_PAGE_COUNT | DEFAULT | [V] |
| SQLITE_MAX_SQL_LENGTH | DEFAULT; RUNTIME | [V] |
| SQLITE_MAX_VARIABLE_NUMBER | DEFAULT; RUNTIME | Builds differ (32766 here, 250000 Homebrew, 500000 Apple) [T], so pin it with sqlite3_limit() when the engine supports parameters [I]. |

Section 5's second list (SQLITE_FTS3_MAX_EXPR_DEPTH, SQLITE_JSON_MAX_DEPTH, SQLITE_MAX_ALLOCATION_SIZE, SQLITE_MAX_MEMORY, SQLITE_MAX_MMAP_SIZE, SQLITE_PRINTF_PRECISION_LIMIT, SQLITE_TRACE_SIZE_LIMIT, YYSTACKDEPTH) repeats entries from sections 4 and 6 and is classified there.

## 6. Operating characteristics (#controlfeatures)

| Option | Rec | Reason |
|---|---|---|
| SQLITE_4_BYTE_ALIGNED_MALLOC | N/A | For systems whose malloc returns 4-byte-aligned pointers [V]. |
| SQLITE_CASE_SENSITIVE_LIKE | OUT | Changes LIKE results [V]. |
| SQLITE_DIRECT_OVERFLOW_READ | DEFAULT (on since 3.45.0) | "this option is enabled by default" [V]. Performance only. |
| SQLITE_HAVE_ISNAN | DEFAULT | "an alias for the HAVE_ISNAN" [V]. |
| SQLITE_MAX_ALLOCATION_SIZE | DEFAULT (0x7ffffeff) | "with a small maximum allocation size, such buggy memory allocation size computations are likely to be spotted sooner" [V], which is a detectability argument. But it caps the largest value the cache can hold, and on the oracle it turns large values into SQLITE_NOMEM where stock succeeds [I]. Revisit only for a test variant. |
| SQLITE_OS_OTHER | N/A | Removes the built-in VFS [V]. A fault-injecting VFS for the simulation can be registered alongside the built-in one without this option [I]. |
| SQLITE_SECURE_DELETE | DEFAULT (off) | "small performance penalty" in exchange for privacy [V]. Not a correctness matter. |
| SQLITE_THREADSAFE=1 | KEEP | "The default is SQLITE_THREADSAFE=1" (Serialized) [V]. The recommended list's =0 is "only usable by applications that never access SQLite from more than one thread at a time" [V]. The binding must not pass SQLITE_OPEN_NOMUTEX, which "can also be used to adjust the threading mode of individual database connections" [V]. It does not: the binding opens with READWRITE, CREATE and EXRESCODE only [S]. |
| SQLITE_TEMP_STORE | DEFAULT (1) | 1 means "Use files by default but allow the PRAGMA temp_store command to override" [V]. For the cache, =2 or =3 would remove temporary-file I/O as a source of local I/O failures, at a memory cost [I]. It does not affect results. Decide together with the I/O-failure story. |
| SQLITE_TRACE_SIZE_LIMIT | N/A | Affects sqlite3_trace() output only [V]. |
| SQLITE_TRUSTED_SCHEMA | DEFAULT now; RUNTIME later | "for best security, systems that implement application-defined SQL functions and/or virtual tables should consider changing the default to OFF" [V]. s4ql has neither. Setting it OFF changes which functions may appear in schema expressions, so decide it when the engine supports DEFAULT or CHECK expressions with functions, and pin it per connection [I]. The CLI already sets it off; the library default is on (inventory note). |
| SQLITE_USE_URI | DEFAULT (off) | Keeping URIs off means a filename cannot switch on options such as `vfs=` or `mode=` [I]. Apple and Homebrew turn it on [T]. |

## 7. Features normally off (#enablefeatures)

The shell column shows the cli.html list of options "recommended in order to provide a full-featured command-line shell" [V].

| Option | Rec | Reason |
|---|---|---|
| SQLITE_ALLOW_URI_AUTHORITY | N/A | UNC filenames [V]. |
| SQLITE_ALLOW_COVERING_INDEX_SCAN | DEFAULT (1) | Setting 0 "will cause results to appear in a different order from legacy" [V]. Keep the stock behaviour. |
| SQLITE_ENABLE_8_3_NAMES | N/A | [V] |
| SQLITE_ENABLE_API_ARMOR | KEEP | "attempts to detect misuse of the SQLite API ... will typically return SQLITE_MISUSE" [V]. Caveat: "does not guarantee that all illegal API usages will be detected ... intended as an aid for application testing and debugging ... Applications should not depend on SQLITE_ENABLE_API_ARMOR for safety" [V]. Tested: sqlite3_prepare_v2(NULL, ...) returns 21 (MISUSE), in both the release and the SQLITE_DEBUG build [T]. |
| SQLITE_ENABLE_ATOMIC_WRITE | OUT | Performance. It changes the commit path ("no rollback journal is created or written") when the filesystem reports atomic writes [V]. |
| SQLITE_ENABLE_BATCH_ATOMIC_WRITE | OUT | F2FS only. "avoids writing the rollback journal" [V]. It changes the crash-consistency path for speed [I]. |
| SQLITE_ENABLE_BYTECODE_VTAB | TEST (optional) | Shell option. "bytecode and tables_used virtual tables" [V]. Diagnostics only. |
| SQLITE_ENABLE_CARRAY | LATER (maybe) | An extension, built in since 3.51.0 [V]. Useful only if the binding binds arrays [I]. |
| SQLITE_ENABLE_COLUMN_METADATA | LATER (maybe) | Adds the sqlite3_column_table_name() family of APIs [V]. |
| SQLITE_ENABLE_DBPAGE_VTAB | OUT | Shell option. The sqlite_dbpage virtual table allows raw page writes, a feature SQLITE_DBCONFIG_DEFENSIVE exists to block (inventory note) [I]. |
| SQLITE_ENABLE_DBSTAT_VTAB | TEST (optional) | Shell option. Diagnostics [V]. |
| SQLITE_ENABLE_DESERIALIZE | N/A | Obsolete: "as of SQLite 3.36.0 ... those interfaces are enabled by default" [V]. |
| SQLITE_ENABLE_EXPLAIN_COMMENTS | DEFAULT (off) | Shell option. Changes EXPLAIN output only [V]. Implied by SQLITE_DEBUG [V, T]. |
| SQLITE_ENABLE_FTS3 | OUT | Shell option. Virtual-table extension, outside the roadmap [I]. |
| SQLITE_ENABLE_FTS3_PARENTHESIS | OUT | Same as above. |
| SQLITE_ENABLE_FTS3_TOKENIZER | OUT | "If hostile actors are able to run the two-argument version ... they could crash or take control of the process" [V]. |
| SQLITE_ENABLE_FTS4 | OUT | Shell option. Same as FTS3. |
| SQLITE_ENABLE_FTS5 | OUT | Shell option. Same as FTS3. |
| SQLITE_ENABLE_GEOPOLY | OUT | Extension [V]. |
| SQLITE_ENABLE_HIDDEN_COLUMNS | OUT | Virtual tables [V]. |
| SQLITE_ENABLE_ICU | OUT | Changes case folding (quirks.html: "Full unicode case folding is supported ... if it is compiled with -DSQLITE_ENABLE_ICU") [V], so results differ from stock. Needs an external library. |
| SQLITE_ENABLE_IOTRACE | OUT | "experimental and may be discontinued" [V]. |
| SQLITE_ENABLE_JSON1 | N/A | "This compile-time option is a no-op" [V]. JSON is built in. |
| SQLITE_ENABLE_LOCKING_STYLE | DEFAULT | Already 1 on Apple and 0 elsewhere [S]. |
| SQLITE_ENABLE_MATH_FUNCTIONS | SET | "This option is automatically added to the Makefile by the configure script on unix platforms" [V], so stock-built SQLite has these functions, while the plain amalgamation does not. The vendored build without it fails with "no such function: sqrt" [T]. Apple and Homebrew have it [T]. It adds 30 functions (acos … trunc) and changes nothing else [T, function-list diff]. It lets the oracle answer when the engine adds math functions. |
| SQLITE_ENABLE_MEMORY_MANAGEMENT | DEFAULT | Needed only for sqlite3_release_memory() [V]. |
| SQLITE_ENABLE_MEMSYS3 | OUT | Alternative allocator [V]. It would hide allocations from the address sanitizer [I]. |
| SQLITE_ENABLE_MEMSYS5 | OUT | Same as above. security.html suggests it "For embedded systems" [V]. |
| SQLITE_ENABLE_NORMALIZE | DEFAULT | Adds an API only [V]. |
| SQLITE_ENABLE_NULL_TRIM | OUT | Changes the on-disk record layout, and such databases "are prone to triggering the e6e962d6b0f06f46 bug in the sqlite3_blob_reopen() interface" [V]. |
| SQLITE_ENABLE_OFFSET_SQL_FUNC | OUT | Shell option. Adds sqlite_offset(), which depends on the file layout [V]. |
| SQLITE_ENABLE_ORDERED_SET_AGGREGATES | N/A | "cannot be used to change the behavior of an amalgamation" [V]. |
| SQLITE_ENABLE_PERCENTILE | LATER | median() and percentile() [V]. Apple and Homebrew have it [T]. Add it when a slice needs it. |
| SQLITE_ENABLE_PREUPDATE_HOOK | DEFAULT | API only. The engine produces its own change files [I]. |
| SQLITE_ENABLE_QPSG | DEFAULT | Plan stability only [V]. |
| SQLITE_ENABLE_RBU | OUT | Extension [V]. |
| SQLITE_ENABLE_RTREE | OUT | Shell option. Extension. |
| SQLITE_ENABLE_SESSION | DEFAULT | Extension [V]. |
| SQLITE_ENABLE_SNAPSHOT | LATER (maybe) | sqlite3_snapshot_get(), _open() and related functions [V]. Worth considering if read connections must pin a WAL snapshot that matches a log position [I]. |
| SQLITE_ENABLE_SORTER_REFERENCES | OUT | Performance, "do not know whether ... will help or hurt" [V]. |
| SQLITE_ENABLE_STMT_SCANSTATUS | TEST (optional) | "not recommended for production builds ... useful for diagnostic and debugging builds" [V]. |
| SQLITE_ENABLE_STMTVTAB | TEST (optional) | Shell option. Diagnostics. |
| SQLITE_RTREE_INT_ONLY | N/A | "deprecated and untested" [V]. |
| SQLITE_ENABLE_SQLLOG | OUT | Needs extra code from the test sources [V]. |
| SQLITE_ENABLE_STAT2 | N/A | No-op [V]. |
| SQLITE_ENABLE_STAT3 | N/A | No-op [V]. |
| SQLITE_ENABLE_STAT4 | OUT | "violates the query planner stability guarantee" [V]. It adds plan variation without a correctness benefit. Homebrew has it [V-measured, inventory note]. |
| SQLITE_ENABLE_TREE_EXPLAIN | N/A | "no longer used" [V]. |
| SQLITE_ENABLE_UPDATE_DELETE_LIMIT | N/A | "may only be used when the library is built from source, not from the amalgamation" [V]. Note: Apple's /usr/bin/sqlite3, built from canonical source, has it [T]. mattn passes it to the amalgamation, where by the docs it cannot work [V, I]. |
| SQLITE_ENABLE_UNKNOWN_SQL_FUNCTION | OUT | Shell option. Affects EXPLAIN only [V]. |
| SQLITE_ENABLE_UNLOCK_NOTIFY | N/A | For shared-cache blocking, which is omitted [I]. |
| SQLITE_INTROSPECTION_PRAGMAS | N/A | "This option is obsolete" [V]. These pragmas are now always present. |
| SQLITE_SOUNDEX | OUT | Off in stock builds [V]. |
| SQLITE_STRICT_SUBTYPE=1 | SET | On the always-tested recommended list. It "causes an error to be raised if an application defined function that does not have the SQLITE_RESULT_SUBTYPE property invokes the sqlite3_result_subtype()" and "is designed to bring this problem to the attention of developers early" [V]. It makes misuse detectable, and it affects only application-defined functions [V]. JSON still worked with it [T]. Homebrew has it [V-measured, inventory note]. |
| SQLITE_USE_ALLOCA | OUT | Performance [V]. |
| SQLITE_USE_FCNTL_TRACE | OUT | VFS logging [V]. |
| SQLITE_USE_SEH | N/A | "Client code should not use this define" [V]. |
| SQLITE_HAVE_ZLIB | N/A | "no effect on the SQLite core" [V]. |
| YYTRACKMAXSTACKDEPTH | TEST (optional) | Diagnostics [V]. |

## 8. Features normally on (#disablefeatures)

| Option | Rec | Reason |
|---|---|---|
| SQLITE_DISABLE_LFS | OUT | Large-file support [V]. |
| SQLITE_DISABLE_DIRSYNC | OUT | Disables the parent-directory sync "to ensure the directory entries are updated immediately on disk" [V]. Keep the durability. |
| SQLITE_DISABLE_FTS3_UNICODE | N/A | FTS [V]. |
| SQLITE_DISABLE_FTS4_DEFERRED | N/A | FTS [V]. |
| SQLITE_DISABLE_INTRINSIC | DEFAULT | Compiler built-ins [V]. |
| SQLITE_DISABLE_PAGECACHE_OVERFLOW_STATS | OUT | Performance [V]. |

## 9. Omit options (#omitfeatures)

The section's warnings apply to every entry [V]:

- "If any of these options are defined, then the same set of SQLITE_OMIT_* options must also be defined when using the Lemon parser generator tool ... these options may only be used when the library is built from canonical source, not from the amalgamation. Some SQLITE_OMIT_* options might work, or appear to work, when used with the amalgamation. But this is not guaranteed."
- "Important Note: The SQLITE_OMIT_* options may not work with the amalgamation."
- "The SQLITE_OMIT_* options are unsupported ... might become a no-op in the next release ... not all SQLITE_OMIT_* options are tested. Some SQLITE_OMIT_* options might cause SQLite to malfunction and/or provide incorrect answers."

howtocompile.html lists the options "that must be known by the code generators", dated "circa SQLite 3.6.20": ENABLE_UPDATE_DELETE_LIMIT, OMIT_ALTERTABLE, ANALYZE, ATTACH, AUTOINCREMENT, CAST, COMPOUND_SELECT, EXPLAIN, FOREIGN_KEY, PRAGMA, REINDEX, SUBQUERY, TEMPDB, TRIGGER, VACUUM, VIEW, VIRTUALTABLE [V]. These are marked "codegen" below and cannot be used with the amalgamation.

| Option | Rec | Reason |
|---|---|---|
| SQLITE_OMIT_ALTERTABLE | N/A (codegen) | |
| SQLITE_OMIT_ANALYZE | N/A (codegen) | |
| SQLITE_OMIT_ATTACH | N/A (codegen) | Use sqlite3_limit(ATTACHED, 0) if needed. |
| SQLITE_OMIT_AUTHORIZATION | OUT | The authorizer is the documented way to make the oracle reject PRAGMA (inventory §4) [V]. |
| SQLITE_OMIT_AUTOINCREMENT | N/A (codegen) | It also silently changes semantics: AUTOINCREMENT columns "behave in the same way as" INTEGER PRIMARY KEY [V]. |
| SQLITE_OMIT_AUTOINIT | OUT | Performance. "the likely result will be a segfault" if initialization is forgotten [V]. |
| SQLITE_OMIT_AUTOMATIC_INDEX | OUT | Plans only. |
| SQLITE_OMIT_AUTORESET | KEEP | sqlite3_step() "will return SQLITE_MISUSE if it is called again after returning anything other than SQLITE_ROW, SQLITE_BUSY, or SQLITE_LOCKED unless there was an intervening call to sqlite3_reset()" [V]. It is not a codegen option and not on the recommended list, so the general warning applies. Tested: step, step, step returns 100, 101, 21 [T]. Apple ships it [T]. |
| SQLITE_OMIT_AUTOVACUUM | OUT | Opens auto-vacuum databases "in read-only mode" [V]. |
| SQLITE_OMIT_BETWEEN_OPTIMIZATION | OUT | Plans only. |
| SQLITE_OMIT_BLOB_LITERAL | OUT | Removes `X'..'` syntax, so the oracle would differ from stock. The engine rejects syntax itself (roadmap principle) [I]. |
| SQLITE_OMIT_BTREECOUNT | N/A | "no-op" [V]. |
| SQLITE_OMIT_BUILTIN_TEST | N/A | "renamed to SQLITE_UNTESTABLE" [V]. |
| SQLITE_OMIT_CASE_SENSITIVE_LIKE_PRAGMA | OUT | Low value: the engine rejects all PRAGMA statements. The pragma is "deprecated" (pragma.html, via the inventory note). |
| SQLITE_OMIT_CAST | N/A (codegen) | |
| SQLITE_OMIT_CHECK | OUT | "The parser will still accept CHECK constraints ... they will just not be enforced" [V]. Silently wrong. |
| SQLITE_OMIT_COMPILEOPTION_DIAGS | OUT | The tests compare compile_options [V, README]. |
| SQLITE_OMIT_COMPLETE | DEFAULT | API only. |
| SQLITE_OMIT_COMPOUND_SELECT | N/A (codegen) | It also breaks multi-row VALUES [V]. |
| SQLITE_OMIT_CTE | OUT | Removes SQL that the roadmap may add. Generated-code status not stated [I]. |
| SQLITE_OMIT_DATETIME_FUNCS | OUT | [I] Same as above. |
| SQLITE_OMIT_DECLTYPE | OUT | On the recommended list, for memory. Keeping sqlite3_column_decltype() can help the harness [I]. |
| SQLITE_OMIT_DEPRECATED | SET | On the always-tested recommended list: "will not help SQLite to run any faster. It will reduce the library footprint, however. And it is the right thing to do" [V]. It removes deprecated APIs such as sqlite3_expired() and sqlite3_transfer_bindings() [V], which makes their use impossible. **Caveat, tested:** on this amalgamation only `default_cache_size` left the pragma list. `count_changes`, `full_column_names` and `short_column_names` still took effect [T], although the docs say they are omitted [V]. Compiles with the binding and the shell [T]. mattn also sets it. |
| SQLITE_OMIT_DESERIALIZE | DEFAULT | API only. |
| SQLITE_OMIT_DISKIO | N/A | "has not been maintained and probably does not work" [V]. |
| SQLITE_OMIT_EXPLAIN | N/A (codegen) | |
| SQLITE_OMIT_FLAG_PRAGMAS | OUT | Would remove `foreign_keys` and the other flag pragmas that the harness uses [I]. |
| SQLITE_OMIT_FLOATING_POINT | OUT | [V] |
| SQLITE_OMIT_FOREIGN_KEY | N/A (codegen) | Contradicts the roadmap. |
| SQLITE_OMIT_GENERATED_COLUMNS | OUT | [I] |
| SQLITE_OMIT_GET_TABLE | DEFAULT | API only. |
| SQLITE_OMIT_HEX_INTEGER | OUT | Syntax divergence from stock. |
| SQLITE_OMIT_INCRBLOB | DEFAULT | API only. Note sqlite3_blob_open does not work on WITHOUT ROWID tables (inventory note). |
| SQLITE_OMIT_INTEGRITY_CHECK | OUT | Needed to verify the cache [I]. |
| SQLITE_OMIT_INTROSPECTION_PRAGMAS | OUT | The harness may use function_list [I]. |
| SQLITE_OMIT_JSON | OUT | JSON is in every stock build [V]. |
| SQLITE_OMIT_LIKE_OPTIMIZATION | OUT | Plans only. |
| SQLITE_OMIT_LOAD_EXTENSION | KEEP | "omits the entire extension loading mechanism" [V]. howtocompile.html uses it directly on the amalgamation: "gcc -DSQLITE_THREADSAFE=0 -DSQLITE_OMIT_LOAD_EXTENSION shell.c sqlite3.c -lm -o sqlite3" [V]. Apple ships it [T]. |
| SQLITE_OMIT_LOCALTIME | OUT | [I] |
| SQLITE_OMIT_LOOKASIDE | DEFAULT | The binding already turns lookaside off at run time under -asan [V, README]. |
| SQLITE_OMIT_MEMORYDB | OUT | The tests use `:memory:` [V, README]. |
| SQLITE_OMIT_OR_OPTIMIZATION | OUT | Plans only. |
| SQLITE_OMIT_PAGER_PRAGMAS | OUT | [I] |
| SQLITE_OMIT_PRAGMA | N/A (codegen) | |
| SQLITE_OMIT_PROGRESS_CALLBACK | OUT | On the recommended list, for speed. The progress handler is the documented guard against long-running statements (security.html) [V]. |
| SQLITE_OMIT_QUICKBALANCE | OUT | Makes SQLite "run slightly slower" [V]. |
| SQLITE_OMIT_REINDEX | N/A (codegen) | |
| SQLITE_OMIT_SCHEMA_PRAGMAS | OUT | The harness checks pragma_table_list [I]. |
| SQLITE_OMIT_SCHEMA_VERSION_PRAGMAS | OUT | [I] |
| SQLITE_OMIT_SHARED_CACHE | KEEP | On the always-tested recommended list, and "recommended for most applications" [V]. So the general omit warning is answered by the docs themselves. |
| SQLITE_OMIT_SEH | N/A | "no effect except when compiling on Windows using MSVC" [V]. |
| SQLITE_OMIT_SUBQUERY | N/A (codegen) | |
| SQLITE_OMIT_TCL_VARIABLE | OUT | Removes `$name` parameters, a syntax divergence from stock. The engine rejects them itself [I]. |
| SQLITE_OMIT_TEMPDB | N/A (codegen) | |
| SQLITE_OMIT_TRACE | DEFAULT | API only. |
| SQLITE_OMIT_TRIGGER | N/A (codegen) | It also "disables enforcement of foreign key constraints" [V]. |
| SQLITE_OMIT_TRUNCATE_OPTIMIZATION | OUT | Performance. |
| SQLITE_OMIT_UTF16 | OUT | "all API functions that return or accept UTF16 encoded text are unavailable" [V]. It is not on the codegen list, and it compiled and ran with the amalgamation [T]. But it also removes `PRAGMA encoding`, which then becomes an unknown pragma [T, S], and it is not in the tested set. An existing UTF-16 file opened by this build failed with "malformed database schema (t) (11)", a corruption error and not "unsupported encoding" [T]. The source forces the encoding to UTF-8 instead of rejecting the file [S]. Without the option, a fresh database can be switched with `PRAGMA encoding='UTF-16le'` [T]. Enforce UTF-8 by opening with sqlite3_open_v2, rejecting PRAGMA in the engine, and asserting pragma_encoding at open [I]. |
| SQLITE_OMIT_VACUUM | N/A (codegen) | |
| SQLITE_OMIT_VIEW | N/A (codegen) | |
| SQLITE_OMIT_VIRTUALTABLE | N/A (codegen) | It would also remove the pragma_* table-valued functions the harness uses [I]. |
| SQLITE_OMIT_WAL | OUT | Parallel reads during a write need WAL [I]. |
| SQLITE_OMIT_WINDOWFUNC | OUT | [I] |
| SQLITE_OMIT_WSD | N/A | "Only use this option if SQLite is being built for an embedded target that does not support WSD" [V]. |
| SQLITE_OMIT_XFER_OPT | OUT | Performance. |
| SQLITE_UNTESTABLE | OUT | "the use of this compile-time option is discouraged". "Branch test coverage drops from 100% down to about 95%" [V]. |
| SQLITE_ZERO_MALLOC | N/A | [V] |

## 10. Analysis and debugging (#debugoptions)

| Option | Rec | Reason |
|---|---|---|
| SQLITE_DEBUG | TEST | "thousands of assert() statements ... turning them on makes SQLite run approximately three times slower. But for testing and analysis, it is useful to turn the assert() statements on. The SQLITE_DEBUG compile-time option does this." It "also enables some other debugging features, such as special PRAGMA statements" [V]. Tested in this run: compiles with the binding (SQLITE_API static) and the shell. It adds 9 pragmas (lock_status, parser_trace, sql_trace, stats, vdbe_addoptrace, vdbe_debug, vdbe_eqp, vdbe_listing, vdbe_trace). It adds 9 SQL functions in the shell build (including json_parse, datedebug, fpdecode). It reports ENABLE_EXPLAIN_COMMENTS. MISUSE returns stay the same [T]. A failed assert aborts the process [I]. |
| SQLITE_MEMDEBUG | TEST (optional) | "instrumented debugging memory allocator ... checks for misuse of dynamically allocated memory" [V]. It compiled and ran together with SQLITE_DEBUG [T]. Useful on macOS, where `go test -asan` is not available [I]. Do not combine with an -asan run [I]. |

## 11. Windows (#win32options)

| Option | Rec | Reason |
|---|---|---|
| SQLITE_WIN32_HEAP_CREATE | N/A | |
| SQLITE_WIN32_MALLOC_VALIDATE | N/A | |

## 12. Linkage (#linkage)

| Option | Rec | Reason |
|---|---|---|
| SQLITE_API | KEEP (`static`, in sqlite.go) | The build needs it: it keeps SQLite's symbols local so that mattn's copy can be linked (README) [V]. "Application developers should not need to worry with these macros" [V], but s4ql has a reason to. |
| SQLITE_APICALL | N/A | |
| SQLITE_CALLBACK | N/A | |
| SQLITE_CDECL | N/A | |
| SQLITE_EXTERN | DEFAULT | |
| SQLITE_STDCALL | N/A | "deprecated" [V]. |
| SQLITE_SYSAPI | N/A | |
| SQLITE_TCLAPI | N/A | |

## Not on compile.html but present in the 3.54.1 source [S]

Do not define these. The page does not document them, so they are outside SQLite's documented contract. Set the matching connection setting at run time instead [I].

- SQLITE_DEFAULT_DEFENSIVE: the matching run-time setting is SQLITE_DBCONFIG_DEFENSIVE.
- SQLITE_DEFAULT_RECURSIVE_TRIGGERS: defined to 0 in the source, which is why "DEFAULT_RECURSIVE_TRIGGERS" shows up in compile_options with the value off.
- SQLITE_DEFAULT_LEGACY_ALTER_TABLE.
- SQLITE_ENABLE_OVERSIZE_CELL_CHECK: the default for cell_size_check. security.html recommends "PRAGMA cell_size_check=ON", which detects corruption early and so suits the cache.
- SQLITE_ENABLE_SETLK_TIMEOUT.

## Cache versus oracle

- **Good for the cache, neutral for the oracle:** SQLITE_MAX_MMAP_SIZE=0. The same holds for TEMP_STORE=2/3 and DEFAULT_WAL_SYNCHRONOUS=1 if they are chosen, but both are run-time or performance decisions.
- **Good for the oracle, neutral for the cache:** ENABLE_MATH_FUNCTIONS and ENABLE_PERCENTILE. Neither changes the result of SQL that the engine already sends [T, I].
- **Good in tests, bad in production:** SQLITE_DEBUG, which is about 3x slower and aborts on a failed assert. The same holds for REVERSE_UNORDERED_SELECTS, MEMDEBUG and STMT_SCANSTATUS.
- **Bad for the oracle, at most a performance gain for the cache:** LIKE_DOESNT_MATCH_BLOBS, MAX_EXPR_DEPTH=0, CASE_SENSITIVE_LIKE, PRINTF_PRECISION_LIMIT, ENABLE_ICU and any syntax-removing OMIT.

## Where the recommended build differs from stock

No single stock exists. The default amalgamation, the configure build, the CLI recommended by cli.html, Apple's /usr/bin/sqlite3 (3.54.0: DQS=3, THREADSAFE=2, ENABLE_UPDATE_DELETE_LIMIT, OMIT_AUTORESET, OMIT_LOAD_EXTENSION, math, percentile, FTS and more [T]), Homebrew, and mattn (no DQS=0 and no FK default [T]) all differ.

SQL-visible differences of the recommended build from the plain amalgamation:

- `"abc"` is an error (the CLI does the same at run time since 3.41.0).
- Foreign keys are enforced.
- The math functions exist (as in configure builds and the CLI).
- `PRAGMA mmap_size=N` returns 0.
- load_extension() is absent.
- `PRAGMA default_cache_size` is unknown.

The API-only differences are AUTORESET, API_ARMOR and STRICT_SUBTYPE. Each SQL-visible option is also a point where the binding differs from the mattn reference, so the cross-check scripts must avoid those points or set the same values at run time [I].
