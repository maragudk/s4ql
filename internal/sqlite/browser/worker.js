'use strict';
// The Worker of the runner's page (index.html): it loads the SQLite WebAssembly build, the bridge,
// the VFS, the file system, and Go's wasm_exec.js, and then runs the Go program, program.wasm, with
// the arguments and environment that the runner serves at config. It tells the runner when the
// program starts. The program's standard output and standard error go back to the runner in order,
// with synchronous requests, and so does its exit code. A Go program on a page of its own needs the
// same scripts, in the order below.
(() => {
  const post = (url, body) => {
    const r = new XMLHttpRequest();
    r.open('POST', url, false);
    r.send(body);
  };

  // Output is sent in batches, at the latest after 100 ms or 64 KiB, and before the exit. Output
  // that starts a test, as go test -v writes it, is sent at once, so that the runner knows which
  // test runs when the program stops returning to the event loop, and the timer cannot fire.
  let queue = [], queued = 0, last = performance.now(), timer = null;
  const run = new TextEncoder().encode('=== RUN ');
  const startsTest = (bytes) => {
    outer: for (let i = 0; i + run.length <= bytes.length; i++) {
      for (let j = 0; j < run.length; j++) if (bytes[i + j] !== run[j]) continue outer;
      return true;
    }
    return false;
  };
  const flush = () => {
    if (timer !== null) { clearTimeout(timer); timer = null; }
    while (queue.length > 0) {
      const fd = queue[0][0];
      let n = 0;
      while (n < queue.length && queue[n][0] === fd) n++;
      post('output?fd=' + fd, new Blob(queue.slice(0, n).map((c) => c[1])));
      queue = queue.slice(n);
    }
    queued = 0;
    last = performance.now();
  };
  self.s4qlOutput = (fd, bytes) => {
    queue.push([fd, bytes.slice()]);
    queued += bytes.length;
    if (queued > 65536 || performance.now() - last > 100 || startsTest(bytes)) flush();
    else if (timer === null) timer = setTimeout(flush, 100);
  };
  const fail = (e) => {
    self.s4qlOutput(2, new TextEncoder().encode('browser: ' + (e && (e.stack || e.message) || e) + '\n'));
    flush();
    post('exit?code=3', '');
  };

  (async () => {
    const config = await (await fetch('config')).json();
    importScripts('sqlite3.js', 'vfs.js', 'bridge.js', 'fs.js', 'wasm_exec.js');
    await s4qlInitSQLite();
    const go = new Go();
    go.argv = ['program.wasm', ...config.args];
    go.env = config.env;
    go.exit = (code) => { flush(); post('exit?code=' + code, ''); };
    const { instance } = await WebAssembly.instantiateStreaming(fetch('program.wasm'), go.importObject);
    post('started', '');
    await go.run(instance);
  })().catch(fail);
  self.addEventListener('error', (e) => fail(e.error || e.message));
  self.addEventListener('unhandledrejection', (e) => fail(e.reason));
})();
