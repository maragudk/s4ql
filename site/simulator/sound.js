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

// figure schedules the figure of the kind on one oscillator into dest, from the time at, and gives
// the voice: its nodes and its end.
function figure(ctx, dest, kind, at) {
  const f = FIGURES[kind];
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
    t = end + GAP;
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
  return {
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
      last.set(kind, at);
      if (!alarm) starts.push(at);
      latest = at;
      return true;
    },
    // silence forgets the figures too, so that the next figure plays whatever was cut.
    silence() {
      const at = ctx.currentTime;
      for (const v of voices) fade(v, at);
      voices.length = 0;
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

// What the page's sound has done, for a check: the figures played and dropped by kind, and the
// latest figures played, each with its kind and the context's time when it was asked for, which
// can be up to some STAGGER before it starts.
export const stats = { played: {}, dropped: {}, recent: [] };

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
