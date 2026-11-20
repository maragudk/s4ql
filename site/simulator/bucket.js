// The tab's database in S3, kept in the tab's sessionStorage, so that a reload finds the bucket as S3
// would hold it, and a new tab starts with a database of its own. The browser keeps sessionStorage
// across reloads of a tab, gives every new tab an empty one, and drops it when the tab closes.
//
// Each tab has a database with a name of its own, such as db-k7m2qa, which is its prefix in the
// engine's store. The items, each a string:
//
//	s4ql:name     the name of the tab's database
//	s4ql:meta     JSON: the engine's storage version, the counter of the workload, the time of the
//	              history, and the number of objects
//	s4ql:m        the manifest
//	s4ql:c:<32>   a commit object, under the 32 bytes of its name
//
// An object is its time, in ms of the history, a comma, and its bytes, one character for each byte,
// as atob gives them. A character below 256 costs one unit of the quota in Chromium and one byte in
// WebKit, against 4/3 for base64. A name is its 32 bytes in the same way.
//
// The writes of one answer happen in one task, with the meta last, so a reload finds the state after
// some answer, never part of one. If a write fails, the page drops the stored database, so that the
// next load starts fresh, and goes on in memory.

const P = "s4ql:";
const NAME = P + "name";
const META = P + "meta";
const MANIFEST = P + "m";
const COMMIT = P + "c:";

// CAP is the number of commit objects at which the page stops: a reload on that many makes the new
// instance catch up for about 4.5 minutes of fake time, two downloads of each, before it serves.
// It is far below the quota: a commit object of the page's workload is about 185 characters here.
export const CAP = 2000;
// BUDGET is the most characters that the page stores, well under the 5,242,880 of Chromium and
// WebKit, as a guard if objects are larger than the page's workload makes them.
export const BUDGET = 3_000_000;

const ALPHABET = "abcdefghjkmnpqrstuvwxyz23456789";

// newName of a database: db- and six letters or digits without the ones that look alike.
export function newName(random = Math.random) {
  let s = "db-";
  for (let i = 0; i < 6; i++) s += ALPHABET[Math.floor(random() * ALPHABET.length)];
  return s;
}

const hexToBytes = (hex) => hex.replace(/../g, (h) => String.fromCharCode(parseInt(h, 16)));
const bytesToHex = (s) => Array.from(s, (c) => c.charCodeAt(0).toString(16).padStart(2, "0")).join("");

// sessionStorage of the tab, or null if the browser has none or refuses it.
function session() {
  try {
    const s = window.sessionStorage;
    s.setItem(P + "probe", "1");
    s.removeItem(P + "probe");
    return s;
  } catch {
    return null;
  }
}

// Keeper of the tab's database: its name, and its objects in storage if the tab has storage. stored
// says whether changes are written.
export class Keeper {
  constructor(storage = session()) {
    this.storage = storage;
    this.stored = !!storage;
    this.sizes = new Map(); // the characters of each item of an object in storage, key and value
    this.chars = 0; // the characters of the items of objects, keys and values
    this.name = (storage && storage.getItem(NAME)) || "";
    if (!/^db-[a-z0-9]{6}$/.test(this.name)) {
      this.name = newName();
      this.#try(() => storage && storage.setItem(NAME, this.name));
    }
  }

  // load the stored bucket of the database, for the engine's start, or null if there is none. A
  // stored database that does not add up is dropped: it says so in note.
  load() {
    this.note = "";
    if (!this.stored) return null;
    const s = this.storage;
    let meta = null;
    try {
      meta = JSON.parse(s.getItem(META) || "null");
    } catch {
      meta = null;
    }
    const objects = [];
    for (let i = 0; i < s.length; i++) {
      const k = s.key(i);
      if (k !== MANIFEST && !k.startsWith(COMMIT)) continue;
      const v = s.getItem(k);
      const comma = v.indexOf(",");
      const key = k === MANIFEST ? `${this.name}/manifest.json` : `${this.name}/commits/${bytesToHex(k.slice(COMMIT.length, COMMIT.length + 1))}/${bytesToHex(k.slice(COMMIT.length))}`;
      objects.push({ key, content: btoa(v.slice(comma + 1)), at: Number(v.slice(0, comma)) });
      this.sizes.set(k, k.length + v.length);
      this.chars += k.length + v.length;
    }
    if (!meta && objects.length === 0) return null;
    if (!meta || meta.n !== objects.length || !objects.every((o) => Number.isFinite(o.at))) {
      this.clear();
      this.note = "damaged";
      return null;
    }
    objects.sort((a, b) => a.at - b.at || (a.key < b.key ? -1 : 1));
    return { version: meta.version, objects, values: meta.values, from: meta.from };
  }

  // keep the changes of an answer of the engine, with its counter, at the time of the history.
  // It returns "" if all is kept or nothing is stored, "full" if the changes would pass the
  // budget, and "failed" if a write failed; in either case the stored database is dropped and
  // nothing more is written.
  keep(answer, version, history) {
    if (!this.stored) return "";
    const writes = new Map();
    for (const c of answer.changes || []) {
      const name = c.key.slice(c.key.lastIndexOf("/") + 1);
      const k = c.key.endsWith("/manifest.json") ? MANIFEST : COMMIT + hexToBytes(name);
      // A later write of the same object in the answer replaces the earlier.
      writes.delete(k);
      writes.set(k, `${c.at},${atob(c.content)}`);
    }
    let chars = this.chars;
    let n = this.sizes.size;
    for (const [k, v] of writes) {
      chars += k.length + v.length - (this.sizes.get(k) || 0);
      if (!this.sizes.has(k)) n++;
    }
    if (chars > BUDGET) return this.#drop("full");
    const result = this.#try(() => {
      for (const [k, v] of writes) this.storage.setItem(k, v);
      this.storage.setItem(META, JSON.stringify({ version, values: answer.values, from: history, n }));
    });
    if (!result) return this.#drop("failed");
    for (const [k, v] of writes) this.sizes.set(k, k.length + v.length);
    this.chars = chars;
    return "";
  }

  // reset the database: clear what is stored, keep its name, and store again if the tab can.
  reset() {
    this.clear();
    this.stored = !!this.storage;
  }

  // clear the stored database, and keep its name.
  clear() {
    this.sizes.clear();
    this.chars = 0;
    if (!this.storage) return;
    this.#try(() => {
      const drop = [];
      for (let i = 0; i < this.storage.length; i++) {
        const k = this.storage.key(i);
        if (k.startsWith(P) && k !== NAME) drop.push(k);
      }
      for (const k of drop) this.storage.removeItem(k);
    });
  }

  #drop(why) {
    this.clear();
    this.stored = false;
    return why;
  }

  #try(f) {
    try {
      f();
      return true;
    } catch {
      return false;
    }
  }
}
