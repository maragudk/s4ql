'use strict';
// A VFS for the official SQLite WebAssembly build, written in JavaScript, for several connections
// in one SQLite instance that share a database in WAL mode. None of the build's own VFSes does
// that: the OPFS ones and memdb refuse WAL or need exclusive locking mode, and the unix ones keep
// their files where Go cannot see them.
//
// The files are those of the in-memory file system in fs.js, through globalThis.s4qlFS, so Go's os
// package sees them too; without fs.js the VFS keeps files of its own. File locks are kept per
// file, by connection, with the rules of SQLite's unix VFS inside one process. The wal-index is
// kept in SQLite's own heap, in regions that every connection to the same database maps, and its
// locks are kept per slot.
//
// s4qlInstallVFS(sqlite3, name, asDefault) registers one VFS of this kind under the name and
// returns {pointer, fault}. Every VFS of this kind works on the same files and locks. Its fault
// state makes operations fail as the C VFS of faultvfs does: ops and files are the operations and
// kinds of file, as SQLITE_OPEN_ flags, that the fault matches, skip the number of matching
// operations that still succeed, times the number that then fail (-1 for all), and code the result
// code of a failure (0 for the usual one); failed counts the failures, and open the files that are
// open through the VFS.
//
// xSleep, which SQLite's busy handler and its retries call, waits in real time and calls
// globalThis.s4qlYield, if it is set, about once a millisecond while it waits. A Go program sets it
// to a Go function, during which Go's runtime runs the goroutines that are ready, so that another
// goroutine can release the lock that SQLite waits for.
(() => {
  const OK = 0, BUSY = 5, CANTOPEN = 14, NOTFOUND = 12, IOERR_DELETE = 2570, IOERR_DELETE_NOENT = 5898;
  const IOERR_READ = 266, IOERR_SHORT_READ = 522, IOERR_WRITE = 778, IOERR_FSYNC = 1034, IOERR_TRUNCATE = 1546, IOERR_FSTAT = 1802, IOERR_LOCK = 3850;
  const OPEN_CREATE = 0x4, OPEN_DELETEONCLOSE = 0x8;
  const KINDS = 0x100 | 0x200 | 0x400 | 0x800 | 0x1000 | 0x2000 | 0x4000 | 0x80000; // every SQLITE_OPEN_ flag of a kind of file
  // The operations of a fault, as bits, as in faultvfs.
  const OP = { open: 1, read: 2, write: 4, truncate: 8, sync: 16, size: 32, lock: 64 };
  // The file lock levels.
  const NONE = 0, SHARED = 1, RESERVED = 2, PENDING = 3, EXCLUSIVE = 4;
  // The flags of xShmLock, which capi does not have.
  const SHM_UNLOCK = 1, SHM_EXCLUSIVE = 8, SHM_NLOCK = 8;
  // What SQLite's unix VFS reports by default: SQLITE_IOCAP_POWERSAFE_OVERWRITE and
  // SQLITE_IOCAP_SUBPAGE_READ, with sectors of 4096 bytes.
  const DEVICE = 0x1000 | 0x8000, SECTOR = 4096;

  const own = { nodes: new Map(), norm: (p) => p, parent: () => null, writableDir: () => true, newFile: () => ({ dir: false, mode: 0o644, data: new Uint8Array(0), size: 0 }) };
  const files = () => globalThis.s4qlFS || own;

  // handles maps the pointer of an sqlite3_file to its handle, for every VFS of this kind.
  const handles = new Map();
  let io = null;

  // state of the locks and the wal-index of the file n.
  const state = (n) => {
    if (!n.sqlite) {
      n.sqlite = {
        shared: new Set(), reserved: null, pending: null, exclusive: null,
        regions: [], shmShared: Array.from({ length: SHM_NLOCK }, () => new Set()), shmExclusive: new Array(SHM_NLOCK).fill(null), shmUsers: 0,
      };
    }
    return n.sqlite;
  };

  // wait for about ms milliseconds without returning to the event loop.
  let cell = null;
  const wait = (ms) => {
    if (cell === null) cell = globalThis.crossOriginIsolated && typeof SharedArrayBuffer === 'function' ? new Int32Array(new SharedArrayBuffer(4)) : false;
    if (cell) { Atomics.wait(cell, 0, 0, ms); return; }
    const end = performance.now() + ms;
    while (performance.now() < end) { /* spin */ }
  };

  // installIO installs the methods of the files once, shared by every VFS of this kind.
  const installIO = (sqlite3) => {
    const { capi, wasm } = sqlite3;
    io = new capi.sqlite3_io_methods();
    io.$iVersion = 2; // with shared memory; the build does not memory-map files
    const handle = (pFile) => handles.get(pFile);
    const methods = {
      xClose(pFile) {
        const h = handle(pFile);
        if (!h) return OK;
        methods.xUnlock(pFile, NONE);
        handles.delete(pFile);
        h.fault.open--;
        if (h.deleteOnClose && h.path !== null && files().nodes.get(h.path) === h.n) files().nodes.delete(h.path);
        return OK;
      },
      xRead(pFile, pDest, n, offset) {
        const h = handle(pFile);
        const rc = h.check(OP.read); if (rc) return rc;
        const d = h.n, at = Number(offset), p = Number(pDest);
        const k = Math.max(0, Math.min(n, d.size - at));
        const heap = wasm.heap8u();
        heap.set(d.data.subarray(at, at + k), p);
        if (k < n) { heap.fill(0, p + k, p + n); return IOERR_SHORT_READ; }
        return OK;
      },
      xWrite(pFile, pSrc, n, offset) {
        const h = handle(pFile);
        const rc = h.check(OP.write); if (rc) return rc;
        const p = Number(pSrc);
        writeAt(h.n, wasm.heap8u().subarray(p, p + n), Number(offset));
        return OK;
      },
      xTruncate(pFile, size) {
        const h = handle(pFile);
        const rc = h.check(OP.truncate); if (rc) return rc;
        resize(h.n, Number(size));
        return OK;
      },
      xSync(pFile, flags) { return handle(pFile).check(OP.sync); },
      xFileSize(pFile, pSize) {
        const h = handle(pFile);
        const rc = h.check(OP.size); if (rc) return rc;
        wasm.poke64(pSize, BigInt(h.n.size));
        return OK;
      },
      xLock(pFile, level) {
        const h = handle(pFile), s = state(h.n);
        const rc = h.check(OP.lock); if (rc) return rc;
        if (level <= h.level) return OK;
        if (level === SHARED) {
          if ((s.pending && s.pending !== h) || (s.exclusive && s.exclusive !== h)) return BUSY;
          s.shared.add(h);
        } else if (level === RESERVED) {
          if ((s.reserved && s.reserved !== h) || (s.pending && s.pending !== h)) return BUSY;
          s.reserved = h;
        } else {
          if (s.pending && s.pending !== h) return BUSY;
          s.pending = h;
          for (const o of s.shared) if (o !== h) { h.level = PENDING; return BUSY; }
          s.exclusive = h;
        }
        h.level = level;
        return OK;
      },
      xUnlock(pFile, level) {
        const h = handle(pFile), s = state(h.n);
        if (level >= h.level) return OK;
        if (level <= SHARED) {
          if (s.reserved === h) s.reserved = null;
          if (s.pending === h) s.pending = null;
          if (s.exclusive === h) s.exclusive = null;
        }
        if (level === NONE) s.shared.delete(h);
        h.level = level;
        return OK;
      },
      xCheckReservedLock(pFile, pOut) {
        const s = state(handle(pFile).n);
        wasm.poke32(pOut, s.reserved || s.pending || s.exclusive ? 1 : 0);
        return OK;
      },
      xFileControl(pFile, op, pArg) { return NOTFOUND; },
      xSectorSize(pFile) { return SECTOR; },
      xDeviceCharacteristics(pFile) { return DEVICE; },
      xShmMap(pFile, iRegion, szRegion, bExtend, pp) {
        const h = handle(pFile), s = state(h.n);
        if (!h.shm) { h.shm = true; s.shmUsers++; }
        if (!s.regions[iRegion]) {
          if (!bExtend) { wasm.poke32(pp, 0); return OK; }
          const p = Number(wasm.alloc(szRegion));
          wasm.heap8u().fill(0, p, p + szRegion);
          s.regions[iRegion] = p;
        }
        wasm.poke32(pp, s.regions[iRegion]);
        return OK;
      },
      xShmLock(pFile, offset, n, flags) {
        const h = handle(pFile), s = state(h.n);
        const unlock = flags & SHM_UNLOCK, exclusive = flags & SHM_EXCLUSIVE;
        if (!unlock) {
          for (let i = offset; i < offset + n; i++) {
            if (s.shmExclusive[i] && s.shmExclusive[i] !== h) return BUSY;
            if (exclusive) for (const o of s.shmShared[i]) if (o !== h) return BUSY;
          }
        }
        for (let i = offset; i < offset + n; i++) {
          if (unlock) {
            s.shmShared[i].delete(h);
            if (s.shmExclusive[i] === h) s.shmExclusive[i] = null;
          } else if (exclusive) {
            s.shmExclusive[i] = h;
          } else {
            s.shmShared[i].add(h);
          }
        }
        return OK;
      },
      xShmBarrier(pFile) {},
      xShmUnmap(pFile, deleteFlag) {
        const h = handle(pFile), s = state(h.n);
        for (let i = 0; i < SHM_NLOCK; i++) {
          s.shmShared[i].delete(h);
          if (s.shmExclusive[i] === h) s.shmExclusive[i] = null;
        }
        if (h.shm) { h.shm = false; s.shmUsers--; }
        // The last connection to unmap frees the wal-index, as a unix VFS deletes the -shm file, so
        // the next one to open the database rebuilds it from the log.
        if (s.shmUsers === 0) {
          for (const p of s.regions) if (p) wasm.dealloc(p);
          s.regions = [];
        }
        return OK;
      },
    };
    sqlite3.vfs.installVfs({ io: { struct: io, methods } });
  };

  const resize = (n, size) => {
    if (globalThis.s4qlFS) return globalThis.s4qlFS.resize(n, size);
    if (size > n.data.length) {
      const d = new Uint8Array(Math.max(size, n.data.length * 2, 65536));
      d.set(n.data.subarray(0, n.size));
      n.data = d;
    } else if (size > n.size) {
      n.data.fill(0, n.size, size);
    }
    n.size = size;
  };
  const writeAt = (n, src, at) => {
    if (at + src.length > n.size) resize(n, at + src.length);
    n.data.set(src, at);
    n.mtime = Date.now();
  };

  globalThis.s4qlInstallVFS = function (sqlite3, name, asDefault) {
    const { capi, wasm } = sqlite3;
    if (!io) installIO(sqlite3);

    const fault = { ops: 0, files: 0, skip: 0, times: 0, code: 0, failed: 0, open: 0 };
    // check whether the operation op on a file of the kind fails, and return the result code of the
    // failure, or OK.
    const check = (kind, op) => {
      if (!(fault.ops & op) || !(fault.files & kind) || fault.times === 0) return OK;
      if (fault.skip > 0) { fault.skip--; return OK; }
      if (fault.times > 0) fault.times--;
      fault.failed++;
      if (fault.code) return fault.code;
      switch (op) {
        case OP.open: return CANTOPEN;
        case OP.read: return IOERR_READ;
        case OP.write: return IOERR_WRITE;
        case OP.truncate: return IOERR_TRUNCATE;
        case OP.sync: return IOERR_FSYNC;
        case OP.size: return IOERR_FSTAT;
        default: return IOERR_LOCK;
      }
    };

    // path in the C string at p, normalized.
    const path = (p) => files().norm(wasm.cstrToJs(p));

    const vfs = new capi.sqlite3_vfs();
    const base = new capi.sqlite3_vfs(capi.sqlite3_vfs_find(null));
    vfs.$iVersion = 2;
    vfs.$szOsFile = capi.sqlite3_file.structInfo.sizeof;
    vfs.$mxPathname = 1024;
    vfs.$xRandomness = base.$xRandomness;
    vfs.$xCurrentTime = base.$xCurrentTime;
    vfs.$xCurrentTimeInt64 = base.$xCurrentTimeInt64;
    vfs.$xGetLastError = base.$xGetLastError;
    base.dispose();

    const encoder = new TextEncoder();
    const methods = {
      xOpen(pVfs, zName, pFile, flags, pOutFlags) {
        const file = new capi.sqlite3_file(pFile);
        file.$pMethods = 0;
        const kind = flags & KINDS;
        const fail = (rc) => { file.dispose(); return rc; };
        const rc = check(kind, OP.open);
        if (rc) return fail(rc);
        const fsx = files();
        let p = null, n;
        if (zName) {
          p = path(zName);
          n = fsx.nodes.get(p);
          if (n && n.dir) return fail(CANTOPEN);
          if (!n) {
            if (!(flags & OPEN_CREATE)) return fail(CANTOPEN);
            const dir = fsx.parent(p);
            if (dir !== null && !fsx.writableDir(dir)) return fail(CANTOPEN);
            n = fsx.newFile();
            fsx.nodes.set(p, n);
          }
        } else {
          n = fsx.newFile();
        }
        handles.set(pFile, {
          path: p, n, level: NONE, shm: false, fault, check: (op) => check(kind, op),
          deleteOnClose: !!(flags & OPEN_DELETEONCLOSE),
        });
        fault.open++;
        file.$pMethods = io.pointer;
        file.dispose();
        if (pOutFlags) wasm.poke32(pOutFlags, flags);
        return OK;
      },
      // xDelete returns what SQLite's unix VFS returns when unlink fails.
      xDelete(pVfs, zName, syncDir) {
        const fsx = files(), p = path(zName);
        const n = fsx.nodes.get(p);
        if (!n) return IOERR_DELETE_NOENT;
        const dir = fsx.parent(p);
        if (n.dir || (dir !== null && !fsx.writableDir(dir))) return IOERR_DELETE;
        fsx.nodes.delete(p);
        return OK;
      },
      xAccess(pVfs, zName, flags, pOut) {
        // As SQLite's unix VFS: a file exists for SQLite if it is not empty, and SQLITE_ACCESS_READWRITE,
        // 1, asks whether it may be read and written.
        const n = files().nodes.get(path(zName));
        const ok = flags === 1 ? n && (n.mode & 0o600) === 0o600 : n && (n.dir || n.size > 0);
        wasm.poke32(pOut, ok ? 1 : 0);
        return OK;
      },
      xFullPathname(pVfs, zName, nOut, pOut) {
        const b = encoder.encode(path(zName));
        if (b.length >= nOut) return CANTOPEN;
        const heap = wasm.heap8u(), p = Number(pOut);
        heap.set(b, p);
        heap[p + b.length] = 0;
        return OK;
      },
      xSleep(pVfs, microseconds) {
        const end = performance.now() + microseconds / 1000;
        for (;;) {
          if (globalThis.s4qlYield) globalThis.s4qlYield();
          const left = end - performance.now();
          if (left <= 0) return microseconds;
          wait(Math.min(left, 1));
        }
      },
    };
    sqlite3.vfs.installVfs({ vfs: { struct: vfs, methods, name, asDefault: !!asDefault } });
    return { pointer: vfs.pointer, fault };
  };
})();
