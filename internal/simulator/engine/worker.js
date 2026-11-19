'use strict';
// The Worker of the simulator's engine. It loads the SQLite WebAssembly build, the bridge, the VFS,
// the file system, and Go's wasm_exec.js, in the order that internal/sqlite/browser/README.md gives,
// and runs the program engine.wasm, the test binary of internal/simulator/engine, which serves the
// messages of the page through the object s4qlEngine below.
//
// The page posts {id, kind, body}, where kind is "run" or "state" for a batch run, or "start",
// "advance", "act", "now", or "end" for a live session, and body is the JSON of the message. The
// Worker posts:
//
//   - {type: "ready", ms} once the engine takes messages, with the milliseconds since the Worker
//     started;
//   - {type: "answer", id, answer, ms, memory} for each message, with the answer as JSON text, the
//     milliseconds that the engine took, and the size in bytes of SQLite's WebAssembly memory after
//     it, which only grows (or the bytes in use, if the build exports sqlite3_memory_used);
//   - {type: "output", fd, text} for what the program writes to standard output or error;
//   - {type: "exit", code} if the program ends, and {type: "error", message} if the Worker fails.
//
// Messages wait in a queue, and the engine takes one at a time.
(() => {
  const started = performance.now();
  const queue = [];
  let ready = false, busy = null;

  const pump = () => {
    if (!ready || busy !== null || queue.length === 0) return;
    const m = queue.shift();
    // Go may handle the message, and call done, before submit returns: a call from JavaScript into
    // Go returns only once every goroutine waits.
    busy = { id: m.id, at: performance.now() };
    if (!self.s4qlEngine.submit(m.id, m.kind, m.body)) {
      busy = null;
      queue.unshift(m);
      setTimeout(pump, 10);
    }
  };

  self.s4qlEngine = {
    ready() {
      ready = true;
      postMessage({ type: 'ready', ms: performance.now() - started });
      setTimeout(pump, 0);
    },
    done(id, answer) {
      const ms = busy ? performance.now() - busy.at : 0;
      busy = null;
      const used = s4qlSQLite.exports.sqlite3_memory_used;
      const memory = typeof used === 'function' ? Number(used()) : s4qlSQLite.sqlite3.wasm.heap8u().length;
      postMessage({ type: 'answer', id, answer, ms, memory });
      // The next message starts once Go has returned from this call.
      setTimeout(pump, 0);
    },
  };

  const decoder = new TextDecoder();
  self.s4qlOutput = (fd, bytes) => {
    postMessage({ type: 'output', fd, text: decoder.decode(bytes) });
  };

  self.addEventListener('message', (e) => {
    const { id, kind, body } = e.data;
    queue.push({ id, kind, body: typeof body === 'string' ? body : JSON.stringify(body) });
    pump();
  });

  const fail = (e) => postMessage({ type: 'error', message: String(e && (e.stack || e.message) || e) });
  self.addEventListener('error', (e) => fail(e.error || e.message));
  self.addEventListener('unhandledrejection', (e) => fail(e.reason));

  (async () => {
    importScripts('sqlite3.js', 'vfs.js', 'bridge.js', 'fs.js', 'wasm_exec.js');
    await s4qlInitSQLite();
    const go = new Go();
    go.argv = ['engine.wasm', '-test.run=^TestEngine$'];
    go.env = {};
    go.exit = (code) => postMessage({ type: 'exit', code });
    const { instance } = await WebAssembly.instantiateStreaming(fetch('engine.wasm'), go.importObject);
    await go.run(instance);
  })().catch(fail);
})();
