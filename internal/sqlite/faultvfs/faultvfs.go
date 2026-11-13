// Package faultvfs is an SQLite VFS that makes file operations fail on demand. It wraps SQLite's
// default VFS, and passes every operation on to it except those that a [Fault] makes fail. Whether
// an operation fails depends only on the fault that is armed and on the operations since it was
// armed, and not on time.
//
// The C code needs only the types of sqlite3.h, and reaches SQLite through the function pointers of
// the default VFS and of its files, so it links no second copy of SQLite.
package faultvfs

/*
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>

#include "../c/sqlite3.h"

// The operations that a fault can make fail, as bits.
enum {
	S4QL_OP_OPEN = 1,
	S4QL_OP_READ = 2,
	S4QL_OP_WRITE = 4,
	S4QL_OP_TRUNCATE = 8,
	S4QL_OP_SYNC = 16,
	S4QL_OP_SIZE = 32,
	S4QL_OP_LOCK = 64,
};

// s4ql_fault is a VFS that wraps real, with the one fault that is armed.
typedef struct s4ql_fault {
	// base must come first: SQLite hands it to the methods, which cast it back.
	sqlite3_vfs base;
	sqlite3_vfs *real;
	char name[64];
	// mu guards the fields after it.
	pthread_mutex_t mu;
	// ops and files are the operations and the kinds of file, as SQLITE_OPEN_ flags, that the
	// fault matches. skip is the number of matching operations that still succeed before the first
	// failure, and times the number that then fail, or -1 for all. code is the result code of a
	// failure, or 0 for the usual one of the operation.
	int ops, files, skip, times, code;
	// failed is the number of operations that failed, and open the number of files that are open.
	int failed, open;
} s4ql_fault;

// s4ql_fault_file is a file of the VFS. The file of the real VFS follows it in the same memory.
typedef struct s4ql_fault_file {
	sqlite3_file base;
	s4ql_fault *vfs;
	// kind of the file, as its SQLITE_OPEN_ flag.
	int kind;
	sqlite3_file *real;
} s4ql_fault_file;

// S4QL_REAL_OFFSET of the real file in the memory of a file, which keeps it aligned.
#define S4QL_REAL_OFFSET ((sizeof(s4ql_fault_file) + 15) / 16 * 16)

// s4ql_fault_check whether the operation op on a file of the kind fails, and return the result
// code of the failure, or SQLITE_OK.
static int s4ql_fault_check(s4ql_fault *v, int kind, int op) {
	int rc = SQLITE_OK;
	pthread_mutex_lock(&v->mu);
	if ((v->ops & op) && (v->files & kind) && v->times != 0) {
		if (v->skip > 0) {
			v->skip--;
		} else {
			if (v->times > 0) {
				v->times--;
			}
			v->failed++;
			rc = v->code;
			if (rc == 0) {
				switch (op) {
				case S4QL_OP_OPEN: rc = SQLITE_CANTOPEN; break;
				case S4QL_OP_READ: rc = SQLITE_IOERR_READ; break;
				case S4QL_OP_WRITE: rc = SQLITE_IOERR_WRITE; break;
				case S4QL_OP_TRUNCATE: rc = SQLITE_IOERR_TRUNCATE; break;
				case S4QL_OP_SYNC: rc = SQLITE_IOERR_FSYNC; break;
				case S4QL_OP_SIZE: rc = SQLITE_IOERR_FSTAT; break;
				default: rc = SQLITE_IOERR_LOCK; break;
				}
			}
		}
	}
	pthread_mutex_unlock(&v->mu);
	return rc;
}

#define S4QL_FILE(f) ((s4ql_fault_file *)(f))
#define S4QL_REAL(f) (S4QL_FILE(f)->real)
#define S4QL_CHECK(f, op) s4ql_fault_check(S4QL_FILE(f)->vfs, S4QL_FILE(f)->kind, op)

static int s4ql_close(sqlite3_file *f) {
	s4ql_fault *v = S4QL_FILE(f)->vfs;
	int rc = S4QL_REAL(f)->pMethods->xClose(S4QL_REAL(f));
	pthread_mutex_lock(&v->mu);
	v->open--;
	pthread_mutex_unlock(&v->mu);
	return rc;
}

static int s4ql_read(sqlite3_file *f, void *p, int n, sqlite3_int64 offset) {
	int rc = S4QL_CHECK(f, S4QL_OP_READ);
	return rc ? rc : S4QL_REAL(f)->pMethods->xRead(S4QL_REAL(f), p, n, offset);
}

static int s4ql_write(sqlite3_file *f, const void *p, int n, sqlite3_int64 offset) {
	int rc = S4QL_CHECK(f, S4QL_OP_WRITE);
	return rc ? rc : S4QL_REAL(f)->pMethods->xWrite(S4QL_REAL(f), p, n, offset);
}

static int s4ql_truncate(sqlite3_file *f, sqlite3_int64 size) {
	int rc = S4QL_CHECK(f, S4QL_OP_TRUNCATE);
	return rc ? rc : S4QL_REAL(f)->pMethods->xTruncate(S4QL_REAL(f), size);
}

static int s4ql_sync(sqlite3_file *f, int flags) {
	int rc = S4QL_CHECK(f, S4QL_OP_SYNC);
	return rc ? rc : S4QL_REAL(f)->pMethods->xSync(S4QL_REAL(f), flags);
}

static int s4ql_file_size(sqlite3_file *f, sqlite3_int64 *size) {
	int rc = S4QL_CHECK(f, S4QL_OP_SIZE);
	return rc ? rc : S4QL_REAL(f)->pMethods->xFileSize(S4QL_REAL(f), size);
}

static int s4ql_lock(sqlite3_file *f, int level) {
	int rc = S4QL_CHECK(f, S4QL_OP_LOCK);
	return rc ? rc : S4QL_REAL(f)->pMethods->xLock(S4QL_REAL(f), level);
}

static int s4ql_unlock(sqlite3_file *f, int level) {
	return S4QL_REAL(f)->pMethods->xUnlock(S4QL_REAL(f), level);
}

static int s4ql_check_reserved_lock(sqlite3_file *f, int *out) {
	return S4QL_REAL(f)->pMethods->xCheckReservedLock(S4QL_REAL(f), out);
}

static int s4ql_file_control(sqlite3_file *f, int op, void *arg) {
	return S4QL_REAL(f)->pMethods->xFileControl(S4QL_REAL(f), op, arg);
}

static int s4ql_sector_size(sqlite3_file *f) {
	return S4QL_REAL(f)->pMethods->xSectorSize(S4QL_REAL(f));
}

static int s4ql_device_characteristics(sqlite3_file *f) {
	return S4QL_REAL(f)->pMethods->xDeviceCharacteristics(S4QL_REAL(f));
}

static int s4ql_shm_map(sqlite3_file *f, int page, int size, int extend, void volatile **p) {
	return S4QL_REAL(f)->pMethods->xShmMap(S4QL_REAL(f), page, size, extend, p);
}

static int s4ql_shm_lock(sqlite3_file *f, int offset, int n, int flags) {
	return S4QL_REAL(f)->pMethods->xShmLock(S4QL_REAL(f), offset, n, flags);
}

static void s4ql_shm_barrier(sqlite3_file *f) {
	S4QL_REAL(f)->pMethods->xShmBarrier(S4QL_REAL(f));
}

static int s4ql_shm_unmap(sqlite3_file *f, int delete) {
	return S4QL_REAL(f)->pMethods->xShmUnmap(S4QL_REAL(f), delete);
}

static int s4ql_fetch(sqlite3_file *f, sqlite3_int64 offset, int n, void **p) {
	return S4QL_REAL(f)->pMethods->xFetch(S4QL_REAL(f), offset, n, p);
}

static int s4ql_unfetch(sqlite3_file *f, sqlite3_int64 offset, void *p) {
	return S4QL_REAL(f)->pMethods->xUnfetch(S4QL_REAL(f), offset, p);
}

// s4ql_methods of a file whose real file has methods of version 3 with shared memory. s4ql_open
// refuses a real file whose methods are of an older version.
static const sqlite3_io_methods s4ql_methods = {
	3,
	s4ql_close,
	s4ql_read,
	s4ql_write,
	s4ql_truncate,
	s4ql_sync,
	s4ql_file_size,
	s4ql_lock,
	s4ql_unlock,
	s4ql_check_reserved_lock,
	s4ql_file_control,
	s4ql_sector_size,
	s4ql_device_characteristics,
	s4ql_shm_map,
	s4ql_shm_lock,
	s4ql_shm_barrier,
	s4ql_shm_unmap,
	s4ql_fetch,
	s4ql_unfetch,
};

// s4ql_methods_without_shm of a file whose real file has no shared memory, as one without locks
// has: SQLite then uses no write-ahead log on it.
static const sqlite3_io_methods s4ql_methods_without_shm = {
	3,
	s4ql_close,
	s4ql_read,
	s4ql_write,
	s4ql_truncate,
	s4ql_sync,
	s4ql_file_size,
	s4ql_lock,
	s4ql_unlock,
	s4ql_check_reserved_lock,
	s4ql_file_control,
	s4ql_sector_size,
	s4ql_device_characteristics,
	0,
	0,
	0,
	0,
	s4ql_fetch,
	s4ql_unfetch,
};

// s4ql_open a file through the real VFS, unless the fault makes it fail.
static int s4ql_open(sqlite3_vfs *vfs, sqlite3_filename name, sqlite3_file *f, int flags, int *out) {
	s4ql_fault *v = (s4ql_fault *)vfs;
	s4ql_fault_file *ff = S4QL_FILE(f);
	ff->base.pMethods = 0;
	ff->vfs = v;
	ff->kind = flags & (SQLITE_OPEN_MAIN_DB | SQLITE_OPEN_TEMP_DB | SQLITE_OPEN_TRANSIENT_DB |
		SQLITE_OPEN_MAIN_JOURNAL | SQLITE_OPEN_TEMP_JOURNAL | SQLITE_OPEN_SUBJOURNAL |
		SQLITE_OPEN_SUPER_JOURNAL | SQLITE_OPEN_WAL);
	ff->real = (sqlite3_file *)((char *)ff + S4QL_REAL_OFFSET);
	int rc = s4ql_fault_check(v, ff->kind, S4QL_OP_OPEN);
	if (rc) {
		return rc;
	}
	rc = v->real->xOpen(v->real, name, ff->real, flags, out);
	// A file whose open failed is closed here, because SQLite does not close every such file, and
	// it then has no methods, so that SQLite closes it nowhere else.
	if (ff->real->pMethods == 0) {
		return rc;
	}
	if (rc != SQLITE_OK || ff->real->pMethods->iVersion < 3) {
		ff->real->pMethods->xClose(ff->real);
		return rc != SQLITE_OK ? rc : SQLITE_CANTOPEN;
	}
	ff->base.pMethods = ff->real->pMethods->xShmMap ? &s4ql_methods : &s4ql_methods_without_shm;
	pthread_mutex_lock(&v->mu);
	v->open++;
	pthread_mutex_unlock(&v->mu);
	return rc;
}

#define S4QL_VFS(vfs) (((s4ql_fault *)(vfs))->real)

static int s4ql_delete(sqlite3_vfs *vfs, const char *name, int syncDir) {
	return S4QL_VFS(vfs)->xDelete(S4QL_VFS(vfs), name, syncDir);
}

static int s4ql_access(sqlite3_vfs *vfs, const char *name, int flags, int *out) {
	return S4QL_VFS(vfs)->xAccess(S4QL_VFS(vfs), name, flags, out);
}

static int s4ql_full_pathname(sqlite3_vfs *vfs, const char *name, int n, char *out) {
	return S4QL_VFS(vfs)->xFullPathname(S4QL_VFS(vfs), name, n, out);
}

static int s4ql_randomness(sqlite3_vfs *vfs, int n, char *out) {
	return S4QL_VFS(vfs)->xRandomness(S4QL_VFS(vfs), n, out);
}

static int s4ql_sleep(sqlite3_vfs *vfs, int microseconds) {
	return S4QL_VFS(vfs)->xSleep(S4QL_VFS(vfs), microseconds);
}

static int s4ql_current_time(sqlite3_vfs *vfs, double *now) {
	return S4QL_VFS(vfs)->xCurrentTime(S4QL_VFS(vfs), now);
}

static int s4ql_get_last_error(sqlite3_vfs *vfs, int n, char *out) {
	return S4QL_VFS(vfs)->xGetLastError(S4QL_VFS(vfs), n, out);
}

static int s4ql_current_time_int64(sqlite3_vfs *vfs, sqlite3_int64 *now) {
	return S4QL_VFS(vfs)->xCurrentTimeInt64(S4QL_VFS(vfs), now);
}

// s4ql_fault_new VFS that wraps base, which has a version of 2 at the least, and is named by its
// address. Nothing is armed. It returns NULL if memory runs out.
static s4ql_fault *s4ql_fault_new(sqlite3_vfs *base) {
	s4ql_fault *v = calloc(1, sizeof(s4ql_fault));
	if (v == 0) {
		return 0;
	}
	v->real = base;
	snprintf(v->name, sizeof(v->name), "s4ql-fault-%p", (void *)v);
	pthread_mutex_init(&v->mu, 0);
	v->base.iVersion = 2;
	v->base.szOsFile = (int)S4QL_REAL_OFFSET + base->szOsFile;
	v->base.mxPathname = base->mxPathname;
	v->base.zName = v->name;
	v->base.xOpen = s4ql_open;
	v->base.xDelete = s4ql_delete;
	v->base.xAccess = s4ql_access;
	v->base.xFullPathname = s4ql_full_pathname;
	v->base.xRandomness = s4ql_randomness;
	v->base.xSleep = s4ql_sleep;
	v->base.xCurrentTime = s4ql_current_time;
	v->base.xGetLastError = s4ql_get_last_error;
	v->base.xCurrentTimeInt64 = s4ql_current_time_int64;
	return v;
}

// s4ql_fault_arm the fault, in place of the one that was armed.
static void s4ql_fault_arm(s4ql_fault *v, int ops, int files, int skip, int times, int code) {
	pthread_mutex_lock(&v->mu);
	v->ops = ops;
	v->files = files;
	v->skip = skip;
	v->times = times;
	v->code = code;
	pthread_mutex_unlock(&v->mu);
}

// s4ql_fault_counts of the VFS: the failures so far, and the files that are open.
static void s4ql_fault_counts(s4ql_fault *v, int *failed, int *open) {
	pthread_mutex_lock(&v->mu);
	*failed = v->failed;
	*open = v->open;
	pthread_mutex_unlock(&v->mu);
}

static void s4ql_fault_free(s4ql_fault *v) {
	pthread_mutex_destroy(&v->mu);
	free(v);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"

	"maragu.dev/s4ql/internal/sqlite"
)

// Op is a set of file operations, as bits.
type Op int

const (
	// Open a file, which fails with SQLITE_CANTOPEN.
	Open Op = C.S4QL_OP_OPEN
	// Read from a file, which fails with SQLITE_IOERR_READ.
	Read Op = C.S4QL_OP_READ
	// Write to a file, which fails with SQLITE_IOERR_WRITE.
	Write Op = C.S4QL_OP_WRITE
	// Truncate a file, which fails with SQLITE_IOERR_TRUNCATE.
	Truncate Op = C.S4QL_OP_TRUNCATE
	// Sync a file, which fails with SQLITE_IOERR_FSYNC.
	Sync Op = C.S4QL_OP_SYNC
	// Size of a file, which fails with SQLITE_IOERR_FSTAT.
	Size Op = C.S4QL_OP_SIZE
	// Lock a file, or raise its lock, which fails with SQLITE_IOERR_LOCK. Unlocking never fails.
	Lock Op = C.S4QL_OP_LOCK
	// AnyOp of the operations above.
	AnyOp = Open | Read | Write | Truncate | Sync | Size | Lock
)

// File is a set of kinds of file, as SQLite's SQLITE_OPEN_ flags name them.
type File int

const (
	// MainDB is the database file.
	MainDB File = C.SQLITE_OPEN_MAIN_DB
	// WAL is the write-ahead log.
	WAL File = C.SQLITE_OPEN_WAL
	// MainJournal is the rollback journal.
	MainJournal File = C.SQLITE_OPEN_MAIN_JOURNAL
	// Temp is every temporary file: of a temporary database, its journal, a statement journal, and a
	// transient database.
	Temp File = C.SQLITE_OPEN_TEMP_DB | C.SQLITE_OPEN_TEMP_JOURNAL | C.SQLITE_OPEN_SUBJOURNAL | C.SQLITE_OPEN_TRANSIENT_DB
	// AnyFile of the kinds above, and a super-journal.
	AnyFile File = MainDB | WAL | MainJournal | Temp | C.SQLITE_OPEN_SUPER_JOURNAL
)

// Fault that a [VFS] makes: the operations in Ops on files of the kinds in Files fail.
type Fault struct {
	Ops   Op
	Files File
	// After is the number of matching operations that succeed before the first failure.
	After int
	// Times is the number of matching operations that then fail. Zero means every one.
	Times int
	// Code is SQLite's result code of a failure. Zero means the usual code of the operation.
	Code int
}

// VFS that wraps SQLite's default VFS and makes the operations of a [Fault] fail. It is registered
// under its own name, and a connection uses it only when it is opened through that name, with
// [sqlite.OpenVFS]. Its methods are safe for concurrent use, except [VFS.Close].
type VFS struct {
	v *C.s4ql_fault
}

// New VFS, registered. Nothing fails until [VFS.Fail] arms a fault. [VFS.Close] frees it.
func New() (*VFS, error) {
	base := sqlite.FindVFS("")
	if base == nil {
		return nil, errors.New("faultvfs: SQLite has no default VFS")
	}
	if (*C.sqlite3_vfs)(base).iVersion < 2 {
		return nil, errors.New("faultvfs: the default VFS of SQLite is older than version 2")
	}
	v := C.s4ql_fault_new((*C.sqlite3_vfs)(base))
	if v == nil {
		return nil, errors.New("faultvfs: out of memory")
	}
	if err := sqlite.RegisterVFS(unsafe.Pointer(&v.base)); err != nil {
		C.s4ql_fault_free(v)
		return nil, err
	}
	return &VFS{v: v}, nil
}

// Name under which the VFS is registered. It panics after [VFS.Close], as [VFS.Fail] and
// [VFS.Failures] do.
func (f *VFS) Name() string {
	f.mustBeOpen()
	return C.GoString(&f.v.name[0])
}

func (f *VFS) mustBeOpen() {
	if f.v == nil {
		panic("faultvfs: the VFS is closed")
	}
}

// Fail the operations of the fault from now on, in place of the fault that was armed before. A
// fault with no operations or no kinds of file disarms it.
func (f *VFS) Fail(fault Fault) {
	f.mustBeOpen()
	times := fault.Times
	if times == 0 {
		times = -1
	}
	C.s4ql_fault_arm(f.v, C.int(fault.Ops), C.int(fault.Files), C.int(fault.After), C.int(times), C.int(fault.Code))
}

// Failures that the VFS has made so far.
func (f *VFS) Failures() int {
	f.mustBeOpen()
	var failed, open C.int
	C.s4ql_fault_counts(f.v, &failed, &open)
	return int(failed)
}

// Close the VFS: unregister it and free it. Every connection that was opened through it must be
// closed first, also one that has no file open, such as a connection to ":memory:", and no
// connection may be opened through it while Close runs. As a check of the first, Close fails, and
// frees nothing, while a file that was opened through the VFS is open. Closing a closed VFS does
// nothing.
func (f *VFS) Close() error {
	if f.v == nil {
		return nil
	}
	var failed, open C.int
	C.s4ql_fault_counts(f.v, &failed, &open)
	if open > 0 {
		return fmt.Errorf("faultvfs: %v files of %v are open", open, f.Name())
	}
	if err := sqlite.UnregisterVFS(unsafe.Pointer(&f.v.base)); err != nil {
		return err
	}
	C.s4ql_fault_free(f.v)
	f.v = nil
	return nil
}
