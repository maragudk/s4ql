'use strict';
// An in-memory file system for a Go program built with GOOS=js, in a browser. It has the callback
// API of Node's fs module that Go's syscall package calls (syscall/fs_js.go), so that os.OpenFile,
// os.CreateTemp, os.MkdirAll, os.ReadDir, os.Rename, and the like work. Load it before Go's
// wasm_exec.js, which keeps a globalThis.fs that is already there.
//
// The files are shared with the SQLite VFS in vfs.js through globalThis.s4qlFS, so that Go's os
// package and SQLite see the same files. Writes to standard output and standard error go to
// globalThis.s4qlOutput(fd, bytes) if it is set, and to console.log line by line otherwise.
//
// Every callback is called from the microtask queue, after the call returns, as Node calls its
// callbacks later. A callback that JavaScript calls during a call from Go runs on the calling
// goroutine, and for a goroutine inside a testing/synctest bubble that panics with "goroutine is
// already bubbled". One consequence: a file operation of Go's os package finishes only once the
// JavaScript stack is empty, so not while Go is inside a call into SQLite.
(() => {
  const S_IFDIR = 0o040000, S_IFREG = 0o100000;
  const constants = { O_RDONLY: 0, O_WRONLY: 1, O_RDWR: 2, O_CREAT: 64, O_EXCL: 128, O_TRUNC: 512, O_APPEND: 1024, O_DIRECTORY: 65536 };

  // nodes maps a normalized absolute path to its node: {dir: true} for a directory, and
  // {dir: false, data, size} for a file, whose content is data[0:size]. Bytes of data at size and
  // after it are not part of the file and may be stale. Each node has its permission bits in mode,
  // which bind as they bind a user who is not root: a directory takes new entries and loses them
  // only with its owner's write bit, and a file opens for reading or writing only with its owner's
  // read or write bit.
  const nodes = new Map();
  let nextIno = 1;
  const umask = 0o22;
  const newDir = (mode = 0o755) => ({ dir: true, mode: mode & 0o7777, ino: nextIno++, mtime: Date.now() });
  const newFile = (mode = 0o644) => ({ dir: false, mode: mode & 0o7777, data: new Uint8Array(0), size: 0, ino: nextIno++, mtime: Date.now() });
  nodes.set('/', newDir(0o777));
  nodes.set('/tmp', newDir(0o777));

  const error = (code) => { const e = new Error(code); e.code = code; return e; };

  // norm makes p absolute, from the root, and removes '.', '..', and empty segments.
  const norm = (p) => {
    const parts = [];
    for (const s of String(p).split('/')) {
      if (s === '' || s === '.') continue;
      if (s === '..') parts.pop(); else parts.push(s);
    }
    return '/' + parts.join('/');
  };
  const parent = (p) => norm(p + '/..');

  // writableDir reports whether the directory at p exists and takes new entries.
  const writableDir = (p) => {
    const n = nodes.get(p);
    return !!n && n.dir && (n.mode & 0o200) !== 0;
  };

  // resize the file n to size bytes, with zeros in any bytes it gains.
  const resize = (n, size) => {
    if (size > n.data.length) {
      const d = new Uint8Array(Math.max(size, n.data.length * 2, 4096));
      d.set(n.data.subarray(0, n.size));
      n.data = d;
    } else if (size > n.size) {
      n.data.fill(0, n.size, size);
    }
    n.size = size;
  };

  // writeAt writes the bytes of src to the file n at the offset pos.
  const writeAt = (n, src, pos) => {
    if (pos + src.length > n.size) resize(n, pos + src.length);
    n.data.set(src, pos);
    n.mtime = Date.now();
  };

  const stat = (n) => ({
    dev: 1, ino: n.ino, mode: (n.dir ? S_IFDIR : S_IFREG) | n.mode, nlink: 1, uid: 0, gid: 0, rdev: 0,
    size: n.dir ? 0 : n.size, blksize: 4096, blocks: n.dir ? 0 : Math.ceil(n.size / 512),
    atimeMs: n.mtime, mtimeMs: n.mtime, ctimeMs: n.mtime, isDirectory: () => n.dir,
  });

  // children of the directory at p, as their paths.
  const children = (p) => {
    const prefix = p === '/' ? '/' : p + '/';
    const out = [];
    for (const k of nodes.keys()) if (k !== '/' && k.startsWith(prefix)) out.push(k);
    return out;
  };

  let pending = new Map(); // fd -> text not yet ended by a line break, for console.log
  const decoder = new TextDecoder('utf-8');
  const output = (fd, bytes) => {
    if (globalThis.s4qlOutput) return globalThis.s4qlOutput(fd, bytes);
    const text = (pending.get(fd) || '') + decoder.decode(bytes);
    const nl = text.lastIndexOf('\n');
    if (nl !== -1) console.log(text.substring(0, nl));
    pending.set(fd, text.substring(nl + 1));
  };

  const fds = new Map(); // fd -> {n, path, append, pos}
  let nextFd = 10;

  // Each method takes its arguments as Node's does, with the callback last, and calls it with an
  // error or null and the result.
  const fs = {
    constants,
    writeSync(fd, buf) { output(fd, buf); return buf.length; },
    open(p, flags, mode, cb) {
      p = norm(p);
      let n = nodes.get(p);
      if (n && (flags & constants.O_CREAT) && (flags & constants.O_EXCL)) return cb(error('EEXIST'));
      if (!n) {
        if (!(flags & constants.O_CREAT)) return cb(error('ENOENT'));
        const dir = nodes.get(parent(p));
        if (!dir) return cb(error('ENOENT'));
        if (!dir.dir) return cb(error('ENOTDIR'));
        if (!writableDir(parent(p))) return cb(error('EACCES'));
        n = newFile(mode & ~umask);
        nodes.set(p, n);
      } else {
        const writes = flags & (constants.O_WRONLY | constants.O_RDWR);
        if ((writes && !(n.mode & 0o200)) || (!(flags & constants.O_WRONLY) && !(n.mode & 0o400))) return cb(error('EACCES'));
      }
      if ((flags & constants.O_DIRECTORY) && !n.dir) return cb(error('ENOTDIR'));
      if (n.dir && (flags & (constants.O_WRONLY | constants.O_RDWR))) return cb(error('EISDIR'));
      if ((flags & constants.O_TRUNC) && !n.dir) resize(n, 0);
      const fd = nextFd++;
      fds.set(fd, { n, path: p, append: !!(flags & constants.O_APPEND), pos: 0 });
      cb(null, fd);
    },
    close(fd, cb) { if (!fds.delete(fd)) return cb(error('EBADF')); cb(null); },
    fstat(fd, cb) { const f = fds.get(fd); if (!f) return cb(error('EBADF')); cb(null, stat(f.n)); },
    stat(p, cb) { const n = nodes.get(norm(p)); if (!n) return cb(error('ENOENT')); cb(null, stat(n)); },
    lstat(p, cb) { const n = nodes.get(norm(p)); if (!n) return cb(error('ENOENT')); cb(null, stat(n)); },
    read(fd, buf, off, len, pos, cb) {
      const f = fds.get(fd); if (!f) return cb(error('EBADF'));
      if (f.n.dir) return cb(error('EISDIR'));
      const at = pos === null ? f.pos : pos;
      const k = Math.max(0, Math.min(len, f.n.size - at));
      buf.set(f.n.data.subarray(at, at + k), off);
      if (pos === null) f.pos = at + k;
      cb(null, k);
    },
    write(fd, buf, off, len, pos, cb) {
      if (fd === 1 || fd === 2) { output(fd, buf.subarray(off, off + len)); return cb(null, len); }
      const f = fds.get(fd); if (!f) return cb(error('EBADF'));
      if (f.n.dir) return cb(error('EISDIR'));
      const at = pos !== null ? pos : f.append ? f.n.size : f.pos;
      writeAt(f.n, buf.subarray(off, off + len), at);
      if (pos === null) f.pos = at + len;
      cb(null, len);
    },
    fsync(fd, cb) { if (!fds.has(fd)) return cb(error('EBADF')); cb(null); },
    ftruncate(fd, len, cb) {
      const f = fds.get(fd); if (!f) return cb(error('EBADF'));
      if (f.n.dir) return cb(error('EISDIR'));
      resize(f.n, len); f.n.mtime = Date.now(); cb(null);
    },
    truncate(p, len, cb) {
      const n = nodes.get(norm(p)); if (!n) return cb(error('ENOENT'));
      if (n.dir) return cb(error('EISDIR'));
      resize(n, len); n.mtime = Date.now(); cb(null);
    },
    unlink(p, cb) {
      p = norm(p); const n = nodes.get(p); if (!n) return cb(error('ENOENT'));
      if (n.dir) return cb(error('EISDIR'));
      if (!writableDir(parent(p))) return cb(error('EACCES'));
      nodes.delete(p); cb(null);
    },
    mkdir(p, perm, cb) {
      p = norm(p);
      if (nodes.has(p)) return cb(error('EEXIST'));
      const dir = nodes.get(parent(p));
      if (!dir) return cb(error('ENOENT'));
      if (!dir.dir) return cb(error('ENOTDIR'));
      if (!writableDir(parent(p))) return cb(error('EACCES'));
      nodes.set(p, newDir(perm & ~umask)); cb(null);
    },
    rmdir(p, cb) {
      p = norm(p); const n = nodes.get(p); if (!n) return cb(error('ENOENT'));
      if (!n.dir) return cb(error('ENOTDIR'));
      if (p === '/') return cb(error('EBUSY'));
      if (children(p).length > 0) return cb(error('ENOTEMPTY'));
      if (!writableDir(parent(p))) return cb(error('EACCES'));
      nodes.delete(p); cb(null);
    },
    readdir(p, cb) {
      p = norm(p); const n = nodes.get(p); if (!n) return cb(error('ENOENT'));
      if (!n.dir) return cb(error('ENOTDIR'));
      const out = [];
      for (const k of children(p)) if (parent(k) === p) out.push(k.substring(p === '/' ? 1 : p.length + 1));
      cb(null, out);
    },
    rename(from, to, cb) {
      from = norm(from); to = norm(to);
      const n = nodes.get(from); if (!n) return cb(error('ENOENT'));
      if (from === to) return cb(null);
      const dir = nodes.get(parent(to));
      if (!dir) return cb(error('ENOENT'));
      if (!dir.dir) return cb(error('ENOTDIR'));
      if (n.dir && (to + '/').startsWith(from + '/')) return cb(error('EINVAL'));
      if (!writableDir(parent(from)) || !writableDir(parent(to))) return cb(error('EACCES'));
      const old = nodes.get(to);
      if (old) {
        if (n.dir && !old.dir) return cb(error('ENOTDIR'));
        if (!n.dir && old.dir) return cb(error('EISDIR'));
        if (old.dir && children(to).length > 0) return cb(error('ENOTEMPTY'));
      }
      const moved = n.dir ? children(from) : [];
      nodes.delete(from);
      nodes.set(to, n);
      for (const k of moved) {
        const c = nodes.get(k);
        nodes.delete(k);
        nodes.set(to + k.substring(from.length), c);
      }
      cb(null);
    },
    utimes(p, atime, mtime, cb) {
      const n = nodes.get(norm(p)); if (!n) return cb(error('ENOENT'));
      n.mtime = mtime * 1000; cb(null);
    },
    chmod(p, mode, cb) { const n = nodes.get(norm(p)); if (!n) return cb(error('ENOENT')); n.mode = mode & 0o7777; cb(null); },
    fchmod(fd, mode, cb) { const f = fds.get(fd); if (!f) return cb(error('EBADF')); f.n.mode = mode & 0o7777; cb(null); },
    chown(p, uid, gid, cb) { if (!nodes.has(norm(p))) return cb(error('ENOENT')); cb(null); },
    fchown(fd, uid, gid, cb) { if (!fds.has(fd)) return cb(error('EBADF')); cb(null); },
    lchown(p, uid, gid, cb) { if (!nodes.has(norm(p))) return cb(error('ENOENT')); cb(null); },
    link(from, to, cb) { cb(error('ENOSYS')); },
    symlink(from, to, cb) { cb(error('ENOSYS')); },
    readlink(p, cb) { cb(error(nodes.has(norm(p)) ? 'EINVAL' : 'ENOENT')); },
  };

  for (const [name, f] of Object.entries(fs)) {
    if (typeof f !== 'function' || name === 'writeSync') continue;
    fs[name] = (...args) => {
      const cb = args[args.length - 1];
      args[args.length - 1] = (...result) => queueMicrotask(() => cb(...result));
      return f(...args);
    };
  }

  globalThis.fs = fs;
  globalThis.path = { resolve: (...ps) => norm(ps.join('/')) };
  globalThis.process = Object.assign(globalThis.process || {}, {
    cwd: () => '/', chdir: () => {}, getuid: () => 0, getgid: () => 0, geteuid: () => 0, getegid: () => 0,
    getgroups: () => [], pid: 1, ppid: 0, umask: () => 0o22,
  });
  globalThis.s4qlFS = { nodes, norm, parent, newFile, writableDir, resize, writeAt };
})();
