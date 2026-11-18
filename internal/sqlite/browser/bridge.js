'use strict';
// The bridge between a Go program built with GOOS=js and the tag s4ql_browser, and the official
// SQLite WebAssembly build, which runs as a second WebAssembly instance beside Go. The two have
// separate memories, so the Go binding in internal/sqlite calls SQLite's exported C functions
// through syscall/js and copies every string and out parameter between them with these helpers.
//
// s4qlInitSQLite() loads the build, which sqlite3.js must have defined as sqlite3InitModule,
// registers the VFS of vfs.js as SQLite's default VFS under the name "s4ql", and sets
// globalThis.s4qlSQLite, which the Go binding reads. Call it, and await it, before the Go program
// starts. Load fs.js before it, so that the VFS keeps its files where Go's os package sees them.
globalThis.s4qlInitSQLite = async function () {
  const sqlite3 = await sqlite3InitModule({ print: (...a) => console.log(...a), printErr: (...a) => console.error(...a) });
  const { wasm } = sqlite3;
  const x = wasm.exports;
  s4qlInstallVFS(sqlite3, 's4ql', true);

  // Every helper returns a number, or nothing, so that Go keeps no reference to a JavaScript object
  // per call (see the type library in sqlite_browser.go). A 64-bit integer goes to Go as a Number
  // when it is exact as one, and as decimal text otherwise, because syscall/js has no BigInt.
  const MAX = BigInt(Number.MAX_SAFE_INTEGER), MIN = -MAX;
  let faults = 0;
  const faultVFSes = new Map(); // pointer -> the struct of a VFS of newFaultVFS

  globalThis.s4qlSQLite = {
    sqlite3,
    // exports of the build: the C functions of SQLite, by their names.
    exports: x,
    // alloc n + 1 bytes with sqlite3_malloc, all zero, and return their address, or 0.
    alloc(n) {
      const p = x.sqlite3_malloc(n + 1);
      if (p) wasm.heap8u().fill(0, p, p + n + 1);
      return p;
    },
    // copyIn copies the first n bytes of the Uint8Array buffer to SQLite's memory at p, and copyOut
    // the n bytes there to the start of buffer.
    copyIn: (buffer, p, n) => { wasm.heap8u().set(buffer.subarray(0, n), p); },
    copyOut: (buffer, p, n) => { buffer.set(wasm.heap8u().subarray(p, p + n)); },
    // strlen of the C string at p.
    strlen(p) {
      const heap = wasm.heap8u();
      let end = p;
      while (heap[end] !== 0) end++;
      return end - p;
    },
    // peek32 at p, as a signed 32-bit integer, such as a pointer.
    peek32: (p) => wasm.peek32(p),
    bindInt64: (stmt, i, v) => x.sqlite3_bind_int64(stmt, i, BigInt(v)),
    columnInt64(stmt, i) {
      const v = x.sqlite3_column_int64(stmt, i);
      return v >= MIN && v <= MAX ? Number(v) : v.toString();
    },
    // newFaultVFS registers one more VFS of vfs.js, with a fault state of its own, for faultvfs.
    newFaultVFS() {
      const name = 's4ql-fault-' + (++faults);
      const { pointer, fault, struct } = s4qlInstallVFS(sqlite3, name, false);
      faultVFSes.set(pointer, struct);
      return { name, pointer, fault };
    },
    // unregisterVFS that newFaultVFS registered, and free it with its name and methods, which no
    // connection may use any more.
    unregisterVFS(pointer) {
      const rc = x.sqlite3_vfs_unregister(pointer);
      if (rc === 0 && faultVFSes.has(pointer)) {
        faultVFSes.get(pointer).dispose();
        faultVFSes.delete(pointer);
      }
      return rc;
    },
  };
  return sqlite3;
};
