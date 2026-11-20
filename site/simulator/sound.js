// The sound of the simulator page: short notes of a square wave on the important events of the
// session, off until the visitor turns it on with the toggle Sound. The page creates no AudioContext
// before that click, and a reload starts with sound off again. The module loads no other file.
//
// Each kind of event has one figure, so that a visitor learns them. The pitches are those of one
// scale, A minor pentatonic from A4 to A6. A figure rises for something that comes back, falls for
// something that goes away, and a fault that the visitor switches is one neutral blip:
//
//	up         an instance opens its database: it starts, restarts, or opens again after a stop
//	down       an instance crashes
//	stop       an instance stops: the visitor's Stop, or its core stops for good
//	takeover   another instance takes the lease
//	failing    the requests go from working to failing
//	recovered  the requests go from failing to working again
//	blip       the visitor switches a fault, moves the clocks, sends the requests elsewhere, or turns
//	           sound on
//	alarm      the checker finds a violation that the design does not excuse
//
// Each request makes a tick besides, so that the page sounds alive between the figures. An insert
// that commits is a barcode being scanned, with the beep of a checkout scanner: one short hard note
// high up. Its pitch is the note of its key, which one mix of the key picks, so that the same key
// always sounds the same and the beeps wander. A select that is served is a much smaller, quieter
// tick in the background, at the note of the newest key that it gives, so that the reads after a
// write echo it until the next, and a read that is behind sounds like an older key; a select without
// rows ticks low. A request
// that fails, is refused, or gets no answer is the scanner's error tone, a low double buzz.
//
// Every figure goes through one master gain. One figure is one voice, and at most VOICES sound at
// once. The site check renders a burst through player offline and holds its peak to at most 0.06,
// or −24 dBFS, and with the alarm, which is louder, to at most 0.1, or −20 dBFS. A figure of a kind
// is dropped within SPACING of the start of the one before it, or while that one still sounds, so
// that a kind never sounds over itself, and every figure but the alarm beyond PER_SECOND a second,
// so that turbo and a burst of events do not become a buzz. Figures that fall together start
// STAGGER apart instead of as a chord.
//
// The page calls hear with each event of the engine, and whether the event is live: while playback
// plays, or in the answer to the visitor's own action, but for the later steps of a reset of the
// clocks, which the page composes of several actions. A paused page sounds only what the visitor
// does. hear follows the events also while sound is off, so that it knows whether the requests fail
// and who holds the lease when sound is turned on. A fault in the sound turns it off for good, and
// never reaches the page.
//
// The ticks have a budget of their own. At most TICK_VOICES sound at once, a tick is dropped within
// TICK_SPACING of the one before it and an error tone within ERROR_SPACING of the one before, so that
// turbo sounds busy and a long outage is not a constant buzz. Ticks never take the place of a figure:
// a tick is dropped while TICK_ROOM or more figures sound, and a figure that starts then fades the
// ticks out, so that the limits of the mix hold with ticks under a burst of figures.
//
// player plays into any BaseAudioContext, so a check can render the figures into an
// OfflineAudioContext with the code path of the page.

export const MASTER = 0.018; // the gain of the master, the peak of one ordinary figure
export const VOICES = 3; // the figures that sound at once
export const SPACING = 0.12; // s, the least time between two figures of one kind
export const PER_SECOND = 4; // the figures in any second, but the alarm
export const STAGGER = 0.05; // s, the least time between the starts of two figures
const LEAD = 0.01; // s, how far ahead of the context's time a figure starts, so that its attack is heard whole
const ATTACK = 0.003; // s
const RELEASE = 0.004; // s
const GAP = 0.015; // s, the silence between two notes of a figure

export const TICK_VOICES = 1; // the ticks that sound at once
export const TICK_SPACING = 0.04; // s, the least time between two ticks
export const ERROR_SPACING = 1.5; // s, the least time between two error tones
export const TICK_ROOM = 2; // the figures that sounding together drop and fade the ticks

const A4 = 440, C5 = 523.25, E5 = 659.26;
const A5 = 880, C6 = 1046.5, D6 = 1174.66, E6 = 1318.51, G6 = 1567.98, A6 = 1760;

// FIGURES by kind: the level relative to MASTER, and the notes, each a pitch in Hz and a length in ms.
export const FIGURES = {
  up: { level: 1, notes: [[A5, 45], [E6, 45]] },
  down: { level: 1, notes: [[E6, 50], [C6, 50], [A5, 60]] },
  stop: { level: 1, notes: [[D6, 45], [A5, 55]] },
  takeover: { level: 1, notes: [[A5, 40], [D6, 40], [A6, 60]] },
  failing: { level: 1, notes: [[C5, 70], [A4, 80]] },
  recovered: { level: 1, notes: [[A4, 60], [E5, 70]] },
  blip: { level: 1, notes: [[D6, 30]] },
  alarm: { level: 2.75, notes: [[G6, 90], [D6, 90], [G6, 90]] },
};

// SCANS are the notes of the keys: the scale of the figures, A minor pentatonic, from E6 to E7, in
// the register of a scanner's piezo.
export const SCANS = [1318.51, 1567.98, 1760, 2093, 2349.32, 2637.02];

// keyNote of a key, from SCANS, by a mix of its bits, so that keys in a row wander over the notes.
export function keyNote(key) {
  let x = Math.abs(Math.trunc(Number(key) || 0)) >>> 0;
  x = Math.imul(x ^ (x >>> 16), 0x45d9f3b) >>> 0;
  x = Math.imul(x ^ (x >>> 16), 0x45d9f3b) >>> 0;
  return SCANS[((x ^ (x >>> 16)) >>> 0) % SCANS.length];
}

// TICKS by kind, as FIGURES; a scan and a select with rows take the note of their key. The scan's
// level puts its peak just below that of a figure. The select is about −42 dBFS, and 20 ms, so that
// its note is heard; without a key it is the low A4. The error tone is two short low notes.
export const TICKS = {
  scan: { level: 0.85, notes: [[SCANS[0], 80]] },
  select: { level: 0.44, notes: [[A4, 20]] },
  error: { level: 0.5, notes: [[330, 45], [330, 45]], gap: 0.035 },
};

// figure schedules the figure of the kind on one oscillator into dest, from the time at, and gives
// the voice: its nodes and its end. A tick is a figure of TICKS.
function figure(ctx, dest, kind, at, figures = FIGURES, hz = 0) {
  const f = hz ? { ...figures[kind], notes: figures[kind].notes.map(([, ms]) => [hz, ms]) } : figures[kind];
  const osc = ctx.createOscillator();
  const gain = ctx.createGain();
  osc.type = "square";
  gain.gain.value = 0; // also before at, so that a fade before the start holds 0
  gain.gain.setValueAtTime(0, at);
  let t = at;
  for (const [hz, ms] of f.notes) {
    const end = t + ms / 1000;
    osc.frequency.setValueAtTime(hz, t);
    gain.gain.setValueAtTime(0, t);
    gain.gain.linearRampToValueAtTime(f.level, t + ATTACK);
    gain.gain.setValueAtTime(f.level, end - RELEASE);
    gain.gain.linearRampToValueAtTime(0, end);
    t = end + (f.gap || GAP);
  }
  osc.connect(gain);
  gain.connect(dest);
  osc.start(at);
  osc.stop(t);
  osc.onended = () => gain.disconnect();
  return { kind, osc, gain, start: at, end: t };
}

// length of the figure of the kind, in s.
const length = (kind) => FIGURES[kind].notes.reduce((s, [, ms]) => s + ms / 1000 + GAP, 0);

// fade a voice out from the time at, in about RELEASE, and stop it. A browser without
// cancelAndHoldAtTime, such as Firefox, glides to 0 from the value that the voice has at at.
function fade(v, at) {
  if (v.end <= at) return;
  const g = v.gain.gain;
  let end = at + RELEASE;
  if (g.cancelAndHoldAtTime) {
    g.cancelAndHoldAtTime(at);
    g.linearRampToValueAtTime(0, end);
  } else {
    g.cancelScheduledValues(at);
    g.setTargetAtTime(0, at, RELEASE / 4);
    end = at + 2 * RELEASE;
  }
  try {
    v.osc.stop(end);
  } catch {
    // It has stopped already.
  }
  v.end = end;
}

// player of figures into dest of the context, with the limits of the package comment. play gives
// whether it plays the figure; silence fades every voice out now.
export function player(ctx, dest) {
  const voices = [];
  const last = new Map(); // the start of the last figure of each kind
  const starts = []; // the starts of the figures in the last second, but the alarm
  let latest = -Infinity; // the start of the last figure
  const ticks = []; // the ticks that sound
  let lastTick = -Infinity, lastError = -Infinity; // the starts of the last tick and error tone
  return {
    // tick gives whether it plays the tick of the kind, within the budget of the ticks, at the pitch
    // hz, or that of the kind.
    tick(kind, hz = 0, at = ctx.currentTime + LEAD) {
      if (!TICKS[kind]) return false;
      for (let i = ticks.length - 1; i >= 0; i--) if (ticks[i].end <= at) ticks.splice(i, 1);
      if (voices.filter((v) => v.end > at).length >= TICK_ROOM || ticks.length >= TICK_VOICES) return false;
      if (at - lastTick < TICK_SPACING || (kind === "error" && at - lastError < ERROR_SPACING)) return false;
      ticks.push(figure(ctx, dest, kind, at, TICKS, hz));
      lastTick = at;
      if (kind === "error") lastError = at;
      return true;
    },
    play(kind, at = ctx.currentTime + LEAD) {
      if (!FIGURES[kind]) return false;
      at = Math.max(at, latest + STAGGER);
      for (let i = voices.length - 1; i >= 0; i--) if (voices[i].end <= at) voices.splice(i, 1);
      while (starts.length && starts[0] <= at - 1) starts.shift();
      if (at - (last.has(kind) ? last.get(kind) : -Infinity) < Math.max(SPACING, length(kind))) return false;
      const alarm = kind === "alarm";
      if (!alarm && (voices.length >= VOICES || starts.length >= PER_SECOND)) return false;
      // The alarm takes the place of the oldest voice.
      while (voices.length >= VOICES) fade(voices.shift(), at);
      voices.push(figure(ctx, dest, kind, at));
      if (voices.length >= TICK_ROOM) {
        for (const v of ticks) fade(v, at);
        ticks.length = 0;
      }
      last.set(kind, at);
      if (!alarm) starts.push(at);
      latest = at;
      return true;
    },
    // silence forgets the figures too, so that the next figure plays whatever was cut.
    silence() {
      const at = ctx.currentTime;
      for (const v of [...voices, ...ticks]) fade(v, at);
      voices.length = 0;
      ticks.length = 0;
      lastTick = lastError = -Infinity;
      last.clear();
      starts.length = 0;
      latest = -Infinity;
    },
  };
}

const instanceOf = (process) => String(process || "").replace(/[0-9]+$/, "");

// listener gives a function that takes the events of a session one at a time, in order, and returns
// the kind of the figure of each, or null for an event without one. It keeps what it needs from the
// events before. The process events that the visitor's own Stop, Start, and Restart cause later are
// silent: the action sounded at once.
export function listener() {
  const expected = new Map(); // by instance: the process events that an action has sounded already
  const expecting = (i) => {
    if (!expected.has(i)) expected.set(i, new Set());
    return expected.get(i);
  };
  // once gives the kind unless the instance expects the event, which it then no longer does.
  const once = (i, what, kind) => (expecting(i).delete(what) ? null : kind);
  let failing = false; // whether the latest request that ended failed, as requestStatus of run.js says
  let leaseWith = ""; // the instance that took the lease last
  return (e) => {
    const i = e.instance || instanceOf(e.process);
    switch (e.kind) {
      case "action":
        if (e.what !== "done") return null;
        switch (e.do) {
          case "crash": return null; // the event crashed of the process sounds
          case "restart":
          case "open":
            expecting(i).add("opened");
            return "up";
          case "close":
            expecting(i).add("closed");
            return "stop";
        }
        return "blip";
      case "process":
        switch (e.what) {
          case "crashed":
            expected.delete(i);
            return "down";
          case "opened": return once(i, "opened", "up");
          case "open_failed":
            expecting(i).delete("opened");
            return null;
          case "stopped":
            expecting(i).add("closed");
            return "stop";
          case "closed": return once(i, "closed", "stop");
        }
        return null;
      case "core": {
        const a = e.attrs || {};
        if (e.event !== "lease" || a["lease.transition"] !== "taken" || a.outcome !== "taken") return null;
        const from = leaseWith;
        leaseWith = i;
        return from && from !== i ? "takeover" : null;
      }
      case "call": {
        if (e.caller !== "client" || e.what === "started") return null;
        const now = e.what !== "committed" && e.what !== "served";
        if (now === failing) return null;
        failing = now;
        return now ? "failing" : "recovered";
      }
      case "check":
        return e.excused ? null : "alarm";
    }
    return null;
  };
}

// tickOf an event: its tick, {kind, hz}, or null, with hz 0 for the pitch of the kind. Each request
// that ends has one: the calls of the engine's client, as the page shows them.
export function tickOf(e) {
  if (e.kind !== "call" || e.caller !== "client" || e.what === "started") return null;
  switch (e.what) {
    case "committed": return { kind: "scan", hz: keyNote(e.value) };
    case "served": return { kind: "select", hz: e.keys && e.keys.length ? keyNote(Math.max(...e.keys)) : 0 };
  }
  return { kind: "error", hz: 0 };
}

// What the page's sound has done, for a check: the figures played and dropped by kind, and the
// latest figures played, each with its kind and the context's time when it was asked for, which
// can be up to some STAGGER before it starts.
export const stats = { played: {}, dropped: {}, recent: [], ticks: {}, ticksDropped: {} };

let ctx = null;
let out = null; // the player of the page
let on = false;
let toggle = null; // the button Sound
const classify = listener();

// hear the event of the engine, and play its figure if sound is on and the event is live.
export function hear(e, live) {
  try {
    const kind = classify(e);
    if (kind && on && live && out) play(kind);
    const tk = tickOf(e);
    if (tk && on && live && out) {
      const n = out.tick(tk.kind, tk.hz) ? stats.ticks : stats.ticksDropped;
      n[tk.kind] = (n[tk.kind] || 0) + 1;
    }
  } catch {
    broken();
  }
}

function play(kind) {
  if (out.play(kind)) {
    stats.played[kind] = (stats.played[kind] || 0) + 1;
    stats.recent.push([kind, ctx.currentTime]);
    if (stats.recent.length > 256) stats.recent.shift();
  } else stats.dropped[kind] = (stats.dropped[kind] || 0) + 1;
}

// broken turns sound off for good, after a fault of the browser's audio.
function broken() {
  on = false;
  out = null;
  if (ctx) {
    const c = ctx;
    Promise.resolve().then(() => c.close()).catch(() => {});
  }
  if (toggle) {
    toggle.disabled = true;
    toggle.setAttribute("aria-pressed", "false");
    toggle.dataset.audio = "unavailable";
  }
}

// wire the toggle Sound, a button with aria-pressed, which the HTML disables until this module has
// loaded. Its data-audio says "none" until the first click that turns sound on creates the
// AudioContext, then the state of the context, and "unavailable" if the browser has none or it
// fails, which also disables the toggle.
export function wire(button) {
  if (!button) return;
  toggle = button;
  button.dataset.audio = "none";
  if (typeof window.AudioContext !== "function") return broken();
  button.disabled = false;
  button.addEventListener("click", () => {
    try {
      if (!ctx) {
        ctx = new AudioContext();
        const master = ctx.createGain();
        master.gain.value = MASTER;
        master.connect(ctx.destination);
        out = player(ctx, master);
        ctx.addEventListener("statechange", () => {
          if (out) button.dataset.audio = ctx.state;
        });
      }
      if (!out) return;
      on = !on;
      button.setAttribute("aria-pressed", String(on));
      button.dataset.audio = ctx.state;
      if (on) {
        ctx.resume().catch(broken);
        play("blip");
      } else {
        out.silence();
        // Suspend once the fades have ended, unless sound is on again by then.
        setTimeout(() => {
          if (!on && out) ctx.suspend().catch(() => {});
        }, 50);
      }
    } catch {
      broken();
    }
  });
}
