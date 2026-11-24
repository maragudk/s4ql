// Constellations in the night sky of the simulator page: while a mouse moves over the page, the few
// stars nearest to it are joined by lines of pixels, and the lines step out when it rests, leaves the
// page, or is over the header. See the CSS of .sky .con in index.html.
//
// The script in the head of the page places the stars, and gives this module the layout of the sky
// as sky.layout, with the event layout, each time it lays the sky out anew: the stars, the
// rectangles out where no star may be, which are the content with a margin, and where the sky is on
// the page. So a move of the pointer reads no layout: the frame after a move finds the star nearest
// to the pointer, by the event's place on the page, and shows that star's constellation.
//
// A constellation belongs to one star, its root, and is found once for each layout: from the root a
// tree grows, a star at a time, by the shortest edge from a star in it to one that is not, up to MOST
// stars. A star may join only if it is within NEAR px of the root, and by an edge at most EDGE px
// long, long enough for a few pixels of line between the gaps, that crosses no rectangle of out,
// so that no line runs behind content. A root whose tree has
// fewer than FEWEST stars shows nothing. The root is the star nearest to the pointer, if it is
// within REACH px; a new one takes over only once it is SWAY px nearer than the root shown, so that a
// pointer between two stars does not flicker between their constellations.
//
// Each constellation is an SVG of its own, as large as its lines, on the device pixels as the stars
// are: lines of pixels of the stars' size by Bresenham's algorithm, from GAP px off the edge of one
// star to GAP px off the other. One that goes steps out in CSS and is removed after OUT ms, and
// when the pointer has not moved for REST ms the one shown goes. Nothing runs while the pointer is
// still or away but that one timer, and nothing runs at all where no mouse hovers, under reduced
// motion, or before the sky has a layout.

const MOST = 4, FEWEST = 3;
const NEAR = 130, EDGE = 110, REACH = 120, SWAY = 8, GAP = 4; // px
const REST = 900, OUT = 800; // ms; OUT is the length of the animation con-out
const FADING = 6; // the most constellations that step out at once; older ones go at once
const svgNS = "http://www.w3.org/2000/svg";

// crosses says whether the segment from a to b meets the rectangle q, [left, top, right, bottom], by
// the clipping of Liang and Barsky. The sides of q may be infinite.
function crosses(a, b, q) {
  const dx = b.x - a.x, dy = b.y - a.y;
  let t0 = 0, t1 = 1;
  for (const [p, d] of [[-dx, a.x - q[0]], [dx, q[2] - a.x], [-dy, a.y - q[1]], [dy, q[3] - a.y]]) {
    if (p === 0) {
      if (d < 0) return false;
      continue;
    }
    const t = d / p;
    if (p < 0) {
      if (t > t1) return false;
      if (t > t0) t0 = t;
    } else {
      if (t < t0) return false;
      if (t < t1) t1 = t;
    }
  }
  return true;
}

// same says whether two layouts of the sky have the same stars and rectangles, as after a change of
// layout that moved nothing that the sky keeps clear of.
function same(a, b) {
  if (!a || a.dpr !== b.dpr || a.stars.length !== b.stars.length || a.out.length !== b.out.length) return false;
  if (a.left !== b.left || a.top !== b.top) return false;
  for (let i = 0; i < a.stars.length; i++) if (a.stars[i].x !== b.stars[i].x || a.stars[i].y !== b.stars[i].y) return false;
  for (let i = 0; i < a.out.length; i++) for (let j = 0; j < 4; j++) if (a.out[i][j] !== b.out[i][j]) return false;
  return true;
}

// grow the tree of the star root in the layout: its edges, as pairs of stars.
function grow(layout, root) {
  const { stars, out } = layout, r = stars[root];
  const dist = (a, b) => Math.hypot(a.x - b.x, a.y - b.y);
  const near = [];
  for (let i = 0; i < stars.length; i++) if (i !== root && dist(stars[i], r) <= NEAR) near.push(i);
  const tree = [root], edges = [];
  while (tree.length < MOST) {
    let best = null, shortest = Infinity;
    for (const b of near) {
      if (tree.includes(b)) continue;
      for (const a of tree) {
        const len = dist(stars[a], stars[b]);
        if (len <= EDGE && len < shortest && len >= stars[a].reach + stars[b].reach + 2 * GAP + 4 && !out.some((q) => crosses(stars[a], stars[b], q))) {
          best = [a, b];
          shortest = len;
        }
      }
    }
    if (!best) break;
    tree.push(best[1]);
    edges.push(best);
  }
  return tree.length >= FEWEST ? edges : [];
}

// picture of the edges in the layout: an SVG of their pixels, placed in the sky.
function picture(layout, edges) {
  const { stars, dpr, k } = layout, cell = k / dpr; // a pixel of the stars, in px
  const on = new Set();
  for (const [a, b] of edges) {
    const A = stars[a], B = stars[b];
    // clear says whether the pixel g, h of the grid, by its centre, is clear of both stars by GAP.
    const clear = (g, h) => {
      const x = (g + 0.5) * cell, y = (h + 0.5) * cell;
      return Math.hypot(x - A.x, y - A.y) >= A.reach + GAP && Math.hypot(x - B.x, y - B.y) >= B.reach + GAP;
    };
    let x0 = Math.floor(A.x / cell), y0 = Math.floor(A.y / cell);
    const x1 = Math.floor(B.x / cell), y1 = Math.floor(B.y / cell);
    const dx = Math.abs(x1 - x0), dy = -Math.abs(y1 - y0), sx = x0 < x1 ? 1 : -1, sy = y0 < y1 ? 1 : -1;
    for (let err = dx + dy; ;) {
      if (clear(x0, y0)) on.add(x0 + "," + y0);
      if (x0 === x1 && y0 === y1) break;
      const e2 = 2 * err;
      if (e2 >= dy) {
        err += dy;
        x0 += sx;
      }
      if (e2 <= dx) {
        err += dx;
        y0 += sy;
      }
    }
  }
  const pixels = [...on].map((s) => s.split(",").map(Number));
  const l = Math.min(...pixels.map((q) => q[0])), t = Math.min(...pixels.map((q) => q[1]));
  const w = Math.max(...pixels.map((q) => q[0])) - l + 1, h = Math.max(...pixels.map((q) => q[1])) - t + 1;
  let d = "";
  for (const [g, j] of pixels) d += `M${(g - l) * k} ${(j - t) * k}h${k}v${k}h${-k}z`;
  const svg = document.createElementNS(svgNS, "svg");
  svg.setAttribute("class", "con");
  svg.setAttribute("shape-rendering", "crispEdges");
  svg.setAttribute("width", w * cell);
  svg.setAttribute("height", h * cell);
  svg.setAttribute("viewBox", `0 0 ${w * k} ${h * k}`);
  svg.style.left = l * cell + "px";
  svg.style.top = t * cell + "px";
  const path = document.createElementNS(svgNS, "path");
  path.setAttribute("d", d);
  svg.appendChild(path);
  return svg;
}

// start the constellations in the sky, the element #sky.
export function start(sky) {
  if (!sky || !window.matchMedia) return;
  const mouse = matchMedia("(hover: hover) and (pointer: fine) and (prefers-reduced-motion: no-preference)");
  let layout = null;
  let trees = new Map(); // the edges of the tree of each root, for this layout
  let shown = null; // the constellation shown: {root, el}
  const fading = new Set(); // the elements of the constellations that step out
  let at = null; // where the pointer is on the page, [x, y], or null if it is away or over the header
  let frame = 0, rest = 0;

  // go: the constellation shown steps out.
  const go = () => {
    if (!shown) return;
    const el = shown.el;
    shown = null;
    el.classList.add("out");
    fading.add(el);
    setTimeout(() => {
      el.remove();
      fading.delete(el);
    }, OUT);
    for (const old of fading) {
      if (fading.size <= FADING) break;
      old.remove();
      fading.delete(old);
    }
  };

  // clear the sky of constellations at once.
  const clear = () => {
    if (shown) shown.el.remove();
    shown = null;
    for (const el of fading) el.remove();
    fading.clear();
    cancelAnimationFrame(frame);
    frame = 0;
    clearTimeout(rest);
  };

  const show = () => {
    frame = 0;
    if (!at || !layout || !mouse.matches) return go();
    const x = at[0] - layout.left, y = at[1] - layout.top, stars = layout.stars;
    let root = -1, nearest = Infinity;
    for (let i = 0; i < stars.length; i++) {
      const d = (stars[i].x - x) ** 2 + (stars[i].y - y) ** 2;
      if (d < nearest) {
        root = i;
        nearest = d;
      }
    }
    nearest = Math.sqrt(nearest);
    if (nearest > REACH) root = -1;
    if (shown && root >= 0 && root !== shown.root) {
      const s = stars[shown.root], d = Math.hypot(s.x - x, s.y - y);
      if (d <= REACH && d - nearest < SWAY) root = shown.root;
    }
    if (shown && root === shown.root) return;
    go();
    if (root < 0) return;
    if (!trees.has(root)) trees.set(root, grow(layout, root));
    const edges = trees.get(root);
    if (!edges.length) return;
    shown = { root, el: sky.appendChild(picture(layout, edges)) };
  };

  const relayout = () => {
    const next = sky.layout;
    if (!next) return;
    if (!same(layout, next)) {
      clear();
      trees = new Map();
    }
    layout = next;
  };

  document.addEventListener("pointermove", (e) => {
    if (e.pointerType === "touch" || !mouse.matches) return;
    at = e.target instanceof Element && e.target.closest("header") ? null : [e.pageX, e.pageY];
    clearTimeout(rest);
    rest = setTimeout(() => {
      at = null;
      go();
    }, REST);
    if (!frame) frame = requestAnimationFrame(show);
  }, { passive: true });
  const away = () => {
    at = null;
    clearTimeout(rest);
    go();
  };
  document.addEventListener("pointerout", (e) => { if (!e.relatedTarget) away(); }, { passive: true });
  window.addEventListener("blur", away);
  mouse.addEventListener("change", () => { if (!mouse.matches) clear(); });
  sky.addEventListener("layout", relayout);
  relayout();
}
