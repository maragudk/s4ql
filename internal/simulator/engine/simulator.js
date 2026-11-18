// The module that a page imports to drive the simulator's engine, which runs in a Worker of
// worker.js. Every file of the engine is served from one directory, the base, which is the
// directory of this module unless start is given another.
//
//   const engine = await Engine.start();
//   const response = await engine.run({ seed: 1, end: 120000, schedule: [] });
//   const state = await engine.state(60000);
//
// run resolves with the response of internal/simulator as an object, and state with the state of
// the last run at a time. Both reject with an Error for a request that the engine refuses. The
// engine takes one message at a time, and later calls wait for earlier ones.
//
// A run whose processes crash leaves their SQLite connections open, as a crashed process does, so
// SQLite's memory grows from run to run. Before a run, the module starts a new Worker once the
// memory has passed recycleBytes, which costs one start of the engine.
export class Engine {
  static recycleBytes = 64 << 20;

  #base;
  #worker = null;
  #ready = null;
  #pending = new Map();
  #next = 1;
  #memory = 0;
  // chain of the calls, so that each starts after the one before has ended, and a new Worker never
  // starts while a call waits on the old one.
  #chain = Promise.resolve();

  // timings of the last start and the last answer, in milliseconds: ready is the time from the
  // start of the Worker until the engine took messages, and run the time the engine took for the
  // last message, without the transfer and parse of the answer.
  timings = { ready: 0, run: 0 };

  constructor(base) {
    this.#base = base;
  }

  static async start(base = new URL('./', import.meta.url)) {
    const engine = new Engine(base);
    await engine.#start();
    return engine;
  }

  #start() {
    const worker = new Worker(new URL('worker.js', this.#base));
    this.#worker = worker;
    this.#memory = 0;
    this.#ready = new Promise((resolve, reject) => {
      const fail = (message) => {
        const error = new Error('simulator engine: ' + message);
        reject(error);
        for (const p of this.#pending.values()) p.reject(error);
        this.#pending.clear();
      };
      worker.addEventListener('error', (e) => fail(e.message || 'the Worker failed'));
      worker.addEventListener('message', (e) => {
        const m = e.data;
        switch (m.type) {
          case 'ready':
            this.timings.ready = m.ms;
            resolve();
            break;
          case 'answer': {
            this.timings.run = m.ms;
            this.#memory = m.memory;
            const p = this.#pending.get(m.id);
            this.#pending.delete(m.id);
            if (!p) break;
            const answer = JSON.parse(m.answer);
            if (answer.error) p.reject(new Error(answer.error));
            else p.resolve(answer);
            break;
          }
          case 'output':
            (m.fd === 2 ? console.error : console.log)(m.text.replace(/\n$/, ''));
            break;
          case 'exit':
            fail('the program exited with code ' + m.code);
            break;
          case 'error':
            fail(m.message);
            break;
        }
      });
    });
    return this.#ready;
  }

  async #send(kind, body) {
    await this.#ready;
    const id = this.#next++;
    return new Promise((resolve, reject) => {
      this.#pending.set(id, { resolve, reject });
      this.#worker.postMessage({ id, kind, body: JSON.stringify(body) });
    });
  }

  // memory of SQLite in bytes after the last answer: the size of its WebAssembly memory, which only
  // grows.
  get memory() {
    return this.#memory;
  }

  #serial(f) {
    const call = this.#chain.then(f);
    this.#chain = call.catch(() => {});
    return call;
  }

  // run the request, and resolve with the response.
  run(request) {
    return this.#serial(async () => {
      await this.#ready;
      if (this.#memory > Engine.recycleBytes) {
        this.#worker.terminate();
        await this.#start();
      }
      return this.#send('run', request);
    });
  }

  // state of the last run at the time t, in milliseconds.
  state(t) {
    return this.#serial(() => this.#send('state', { t }));
  }

  terminate() {
    this.#worker.terminate();
  }
}
