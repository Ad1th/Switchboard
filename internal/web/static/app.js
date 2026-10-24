// Switchboard control panel. Every request through the proxy is drawn as a
// patch cord from a caller jack on the shelf to the backend line it reached.

const $ = (sel, el = document) => el.querySelector(sel);
const reduceMotion = matchMedia("(prefers-reduced-motion: reduce)").matches;

const STRATEGIES = {
  roundrobin: { label: "Round robin", note: "Each line takes the next call in turn." },
  weighted: { label: "Weighted", note: "Lines take calls in proportion to their weight." },
  leastconn: { label: "Least busy", note: "The line with the fewest calls in progress answers." },
  p2c: { label: "Two choices", note: "Pick two lines at random, send the call to the one with the shorter expected wait." },
  hash: { label: "Sticky", note: "The same caller always reaches the same line while it stays up." },
};

const COLORS = {
  ok: "#b5895a",
  slow: "#e09a2d",
  fail: "#e2442f",
  refused: "#7d8c99",
};

let state = null;
let token = "";
try { token = localStorage.getItem("switchboard-token") || ""; } catch {}

// ---------------------------------------------------------------- api

const slip = $("#slip");
let slipTimer;
function notify(message, isError = false) {
  slip.textContent = message;
  slip.classList.toggle("error", isError);
  slip.classList.add("show");
  clearTimeout(slipTimer);
  slipTimer = setTimeout(() => slip.classList.remove("show"), 3200);
}

async function api(method, path, body) {
  const headers = { "Content-Type": "application/json" };
  if (token) headers["X-Admin-Token"] = token;
  const res = await fetch(`api/${path}`, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = await res.json().catch(() => ({}));
  if (res.status === 401) {
    if (await askToken()) return api(method, path, body);
    throw new Error("Settings are locked. Enter the operator token to change them.");
  }
  if (!res.ok) throw new Error(data.error || `${res.status} ${res.statusText}`);
  if (data && data.backends) applyState(data);
  return data;
}

function act(promise, done) {
  promise.then(() => done && notify(done)).catch((err) => notify(err.message, true));
}

const tokenBox = $("#tokenbox");
function askToken() {
  return new Promise((resolve) => {
    const form = $("#token-form");
    const input = $("#tk-input");
    input.value = "";
    const finish = (ok) => {
      form.onsubmit = null;
      $("#tk-cancel").onclick = null;
      tokenBox.close();
      resolve(ok);
    };
    form.onsubmit = (e) => {
      e.preventDefault();
      token = input.value.trim();
      try { localStorage.setItem("switchboard-token", token); } catch {}
      finish(true);
    };
    $("#tk-cancel").onclick = () => finish(false);
    tokenBox.showModal();
  });
}

// ---------------------------------------------------------------- meters

function polar(cx, cy, r, deg) {
  const a = ((deg - 90) * Math.PI) / 180;
  return [cx + r * Math.cos(a), cy + r * Math.sin(a)];
}

class Meter {
  constructor(el, { max, majors, redFrom, format }) {
    this.max = max;
    this.redFrom = redFrom;
    this.format = format;
    this.value = 0;
    this.shown = 0;
    this.velocity = 0;
    const ns = "http://www.w3.org/2000/svg";
    const svg = document.createElementNS(ns, "svg");
    svg.setAttribute("viewBox", "0 0 160 96");
    svg.setAttribute("role", "img");
    const cx = 80, cy = 86, r = 66, sweep = 55;
    const add = (tag, attrs) => {
      const node = document.createElementNS(ns, tag);
      for (const [k, v] of Object.entries(attrs)) node.setAttribute(k, v);
      svg.appendChild(node);
      return node;
    };
    add("path", { class: "face", d: "M6 90 Q6 6 80 6 Q154 6 154 90 Z" });
    if (redFrom !== undefined) {
      const a0 = -sweep + (2 * sweep * redFrom) / max;
      const [x0, y0] = polar(cx, cy, r - 4, a0);
      const [x1, y1] = polar(cx, cy, r - 4, sweep);
      add("path", { class: "redzone", d: `M${x0} ${y0} A${r - 4} ${r - 4} 0 0 1 ${x1} ${y1}` });
    }
    for (let i = 0; i <= majors * 5; i++) {
      const major = i % 5 === 0;
      const a = -sweep + (2 * sweep * i) / (majors * 5);
      const [x0, y0] = polar(cx, cy, r, a);
      const [x1, y1] = polar(cx, cy, r - (major ? 10 : 5), a);
      add("line", { class: major ? "tick major" : "tick", x1: x0, y1: y0, x2: x1, y2: y1 });
      if (major) {
        const [tx, ty] = polar(cx, cy, r - 19, a);
        add("text", { class: "num", x: tx, y: ty + 3 }).textContent = Math.round((max * i) / (majors * 5));
      }
    }
    this.reading = add("text", { class: "reading", x: cx, y: 70 });
    this.needle = add("line", { class: "needle", x1: cx, y1: cy, x2: cx, y2: cy - r + 6 });
    add("circle", { class: "pivot", cx, cy, r: 6 });
    this.svg = svg;
    this.cx = cx; this.cy = cy; this.sweep = sweep;
    el.prepend(svg);
    this.set(0);
  }
  set(v) {
    this.value = v;
    this.reading.textContent = this.format(v);
    this.svg.setAttribute("aria-label", `${this.format(v)}`);
  }
  step(dt) {
    // A damped spring, so the needle swings and settles like a real one.
    const target = Math.min(this.value, this.max * 1.04);
    if (reduceMotion) this.shown = target;
    else {
      this.velocity += (target - this.shown) * 60 * dt;
      this.velocity *= Math.exp(-9 * dt);
      this.shown += this.velocity * dt;
    }
    const a = -this.sweep + (2 * this.sweep * Math.max(0, this.shown)) / this.max;
    this.needle.setAttribute("transform", `rotate(${a} ${this.cx} ${this.cy})`);
  }
}

const meters = {
  rps: new Meter($("#meter-rps"), { max: 500, majors: 5, format: (v) => Math.round(v) }),
  p95: new Meter($("#meter-p95"), { max: 1000, majors: 5, redFrom: 600, format: (v) => Math.round(v) }),
  err: new Meter($("#meter-err"), { max: 100, majors: 4, redFrom: 25, format: (v) => `${v.toFixed(v < 10 ? 1 : 0)}` }),
};

// Recent finished calls, for the meters and the chart recorder.
const recent = [];
function recentWindow(ms) {
  const cutoff = Date.now() - ms;
  while (recent.length && recent[0].t < cutoff - 10000) recent.shift();
  return recent.filter((e) => e.t >= cutoff);
}
function quantile(sorted, q) {
  if (!sorted.length) return 0;
  return sorted[Math.min(sorted.length - 1, Math.floor(q * sorted.length))];
}

// ---------------------------------------------------------------- lines

const linesEl = $("#lines");
const lineEls = new Map();
let layoutDirty = true;

function lineState(b) {
  if (!b.healthy) return "down";
  if (b.breaker === "open") return "open";
  if (b.breaker === "half-open") return "testing";
  if (b.draining || b.weight === 0) return "draining";
  return "ok";
}

const shutterText = { down: "No answer", open: "Circuit open", testing: "Testing" };

function hostPort(url) {
  try { return new URL(url).host; } catch { return url; }
}

function renderLines(backends) {
  const seen = new Set();
  for (const b of backends) {
    seen.add(b.id);
    let line = lineEls.get(b.id);
    if (!line) {
      const li = document.createElement("li");
      li.className = "line";
      li.dataset.id = b.id;
      li.innerHTML = `
        <span class="lamp"></span>
        <button type="button" class="jack"><span class="shutter"></span></button>
        <div class="designation"><b></b><span class="host"></span><br><span class="tag" hidden></span></div>
        <div class="line-gauge" aria-hidden="true"><span></span></div>
        <span class="line-p95"></span>`;
      li.querySelector(".jack").addEventListener("click", () => openLine(b.id));
      linesEl.appendChild(li);
      line = {
        el: li,
        lamp: li.querySelector(".lamp"),
        jack: li.querySelector(".jack"),
        shutter: li.querySelector(".shutter"),
        name: li.querySelector("b"),
        host: li.querySelector(".host"),
        tag: li.querySelector(".tag"),
        gauge: li.querySelector(".line-gauge span"),
        p95: li.querySelector(".line-p95"),
      };
      lineEls.set(b.id, line);
      layoutDirty = true;
    }
    const st = lineState(b);
    line.el.dataset.state = st;
    line.name.textContent = `Line ${b.id}`;
    line.host.textContent = hostPort(b.url);
    line.shutter.textContent = shutterText[st] || "";
    const tag = b.draining ? "Draining" : b.weight === 0 ? "Parked" : state.strategy === "weighted" || b.weight !== 1 ? `Weight ${b.weight}` : "";
    line.tag.hidden = !tag;
    line.tag.textContent = tag;
    line.tag.classList.toggle("warn", b.draining || b.weight === 0);
    line.lamp.style.setProperty("--glow", Math.min(1, b.active / 6).toFixed(2));
    line.gauge.style.width = `${Math.min(100, (b.p95_ms / 600) * 100)}%`;
    line.p95.textContent = b.served ? `p95 ${Math.round(b.p95_ms)} ms, ${b.served} ${b.served === 1 ? "call" : "calls"}` : "No calls yet";
    line.jack.setAttribute("aria-label", `Line ${b.id}, ${hostPort(b.url)}, ${shutterText[st] || (st === "draining" ? "draining" : "in service")}. Open line card.`);
  }
  for (const [id, line] of lineEls) {
    if (!seen.has(id)) {
      line.el.remove();
      lineEls.delete(id);
      layoutDirty = true;
    }
  }
  if (!backends.length && !$(".empty-field")) {
    linesEl.insertAdjacentHTML("beforeend", `<li class="empty-field">No lines installed.</li>`);
  }
}

function flashLine(id, failed) {
  const line = lineEls.get(id);
  if (!line || !failed) return;
  line.el.classList.add("hit-fail");
  clearTimeout(line.flash);
  line.flash = setTimeout(() => line.el.classList.remove("hit-fail"), 380);
}

// ---------------------------------------------------------------- callers

const CALLERS = 36;
const callersEl = $("#callers");
const callerEls = [];
const callerLoad = new Array(CALLERS).fill(0);
for (let i = 0; i < CALLERS; i++) {
  const c = document.createElement("span");
  c.className = "caller";
  callersEl.appendChild(c);
  callerEls.push(c);
}
function callerIndex(addr) {
  let h = 2166136261;
  for (let i = 0; i < addr.length; i++) h = Math.imul(h ^ addr.charCodeAt(i), 16777619);
  return (h >>> 0) % CALLERS;
}

// ---------------------------------------------------------------- cords

const cabinet = $("#cabinet");
const canvas = $("#cords");
const ctx = canvas.getContext("2d");
let dpr = 1;
let jackPos = new Map();
let callerPos = [];
const cords = [];
const maxCords = () => (cabinet.clientWidth < 700 ? 36 : 110);

function layout() {
  const rect = cabinet.getBoundingClientRect();
  dpr = Math.min(2, window.devicePixelRatio || 1);
  canvas.width = Math.round(rect.width * dpr);
  canvas.height = Math.round(rect.height * dpr);
  const rel = (el, yFrac = 0.5) => {
    const r = el.getBoundingClientRect();
    return { x: r.left - rect.left + r.width / 2, y: r.top - rect.top + r.height * yFrac };
  };
  jackPos = new Map([...lineEls].map(([id, l]) => [id, rel(l.jack)]));
  callerPos = callerEls.map((c) => rel(c));
  layoutDirty = false;
}
new ResizeObserver(() => (layoutDirty = true)).observe(cabinet);
document.fonts?.ready.then(() => (layoutDirty = true));

function cordColor(ev, slowMs) {
  if (!ev.backend || ev.status === 429 || ev.status === 503) return COLORS.refused;
  if (ev.status >= 500 || ev.status === 499) return COLORS.fail;
  if (ev.ms > slowMs) return COLORS.slow;
  return COLORS.ok;
}

function addCord(ev, delay, slowMs) {
  if (cords.length >= maxCords()) return;
  const from = callerIndex(ev.client);
  const refused = !ev.backend || ev.status === 429;
  const hold = refused ? 0 : Math.min(2400, Math.max(320, ev.ms * 1.6));
  cords.push({
    from,
    to: ev.backend,
    color: cordColor(ev, slowMs),
    failed: ev.status >= 500,
    rerouted: ev.attempts > 1,
    refused,
    start: performance.now() + delay,
    hold,
    swing: (Math.random() - 0.5) * 2,
    landed: false,
  });
  callerLoad[from]++;
}

// Cubic curve from the caller jack up to the line jack, with slack that
// droops below the straight line like a weighted cloth cord.
function cordCurve(a, b, swing, t) {
  const dx = b.x - a.x;
  const dy = a.y - b.y;
  const slack = Math.max(40, dy * 0.28) + Math.sin(t * 2.2 + swing * 3) * 5 * swing;
  return [
    a,
    { x: a.x + dx * 0.15 + swing * 18, y: a.y - dy * 0.1 + slack * 0.6 },
    { x: b.x - dx * 0.1 + swing * 12, y: b.y + dy * 0.55 + slack },
    b,
  ];
}

function bez(p, t) {
  const u = 1 - t;
  return {
    x: u * u * u * p[0].x + 3 * u * u * t * p[1].x + 3 * u * t * t * p[2].x + t * t * t * p[3].x,
    y: u * u * u * p[0].y + 3 * u * u * t * p[1].y + 3 * u * t * t * p[2].y + t * t * t * p[3].y,
  };
}

function drawCord(c, now) {
  const a = callerPos[c.from];
  let b = c.to ? jackPos.get(c.to) : null;
  if (!a) return true;
  const age = now - c.start;
  if (age < 0) return false;
  const reach = 240, back = 260;

  let extent; // how much of the cord is out of the shelf, 0..1
  if (c.refused) {
    // Lifted toward the board, then dropped: the call was never connected.
    b = { x: a.x + c.swing * 60, y: a.y - 110 };
    if (age > 520) return true;
    extent = Math.sin((age / 520) * Math.PI);
  } else {
    if (!b) return true;
    const total = reach + c.hold + back;
    if (age > total) return true;
    if (age < reach) extent = easeOut(age / reach);
    else if (age < reach + c.hold) {
      extent = 1;
      if (!c.landed) {
        c.landed = true;
        flashLine(c.to, c.failed);
      }
    } else extent = 1 - easeIn((age - reach - c.hold) / back);
  }

  const pts = cordCurve(a, b, c.swing, now / 1000);
  const steps = 26;
  const path = new Path2D();
  let last = a, prev = a;
  path.moveTo(a.x, a.y);
  for (let i = 1; i <= steps; i++) {
    const t = (i / steps) * extent;
    prev = last;
    last = bez(pts, t);
    path.lineTo(last.x, last.y);
  }

  ctx.lineCap = "round";
  ctx.lineJoin = "round";
  ctx.strokeStyle = "rgba(0,0,0,.45)";
  ctx.lineWidth = 6;
  ctx.stroke(path);
  ctx.strokeStyle = c.color;
  ctx.lineWidth = 3.6;
  ctx.stroke(path);
  // cloth braid
  ctx.setLineDash([2, 4]);
  ctx.strokeStyle = "rgba(255,240,210,.35)";
  ctx.lineWidth = 1.2;
  ctx.stroke(path);
  ctx.setLineDash([]);
  if (c.rerouted) {
    ctx.setLineDash([7, 7]);
    ctx.strokeStyle = "rgba(240,213,138,.9)";
    ctx.lineWidth = 1.6;
    ctx.stroke(path);
    ctx.setLineDash([]);
  }

  // brass plug at the leading end
  const ang = Math.atan2(last.y - prev.y, last.x - prev.x);
  ctx.save();
  ctx.translate(last.x, last.y);
  ctx.rotate(ang);
  const g = ctx.createLinearGradient(0, -4, 0, 4);
  g.addColorStop(0, "#f0d58a");
  g.addColorStop(0.5, "#c9a453");
  g.addColorStop(1, "#6e521d");
  ctx.fillStyle = g;
  ctx.fillRect(-14, -4, 14, 8);
  ctx.fillStyle = "#1c1714";
  ctx.fillRect(-18, -3.2, 4, 6.4);
  ctx.restore();
  return false;
}

const easeOut = (t) => 1 - Math.pow(1 - t, 3);
const easeIn = (t) => t * t * t;

// ---------------------------------------------------------------- chart recorder

const chart = $("#chart");
const cctx = chart.getContext("2d");
const samples = []; // { t, p50, p95, rps }
const PX_PER_SEC = 6;

function sampleChart() {
  const win = recentWindow(2000).filter((e) => e.backend && e.status !== 429);
  const ms = win.map((e) => e.ms).sort((x, y) => x - y);
  const rps = state ? state.rps[state.rps.length - 1] || 0 : 0;
  samples.push({ t: Date.now(), p50: quantile(ms, 0.5), p95: quantile(ms, 0.95), rps });
  while (samples.length > 600) samples.shift();
}
let lastSample = 0;

function drawChart() {
  const w = chart.clientWidth, h = chart.clientHeight;
  if (chart.width !== Math.round(w * dpr)) {
    chart.width = Math.round(w * dpr);
    chart.height = Math.round(h * dpr);
  }
  cctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  cctx.clearRect(0, 0, w, h);
  const now = Date.now();
  const left = 34, right = w - 34, top = 6, bottom = h - 18;

  // graph paper scrolls with time
  const offset = ((now / 1000) * PX_PER_SEC) % 30;
  cctx.lineWidth = 1;
  for (let x = right + 30 - offset; x > left; x -= 6) {
    const major = Math.abs(((x - right - 30 + offset) / 6) % 5) < 0.01;
    cctx.strokeStyle = major ? "rgba(43,110,80,.35)" : "rgba(43,110,80,.13)";
    cctx.beginPath(); cctx.moveTo(x, top); cctx.lineTo(x, bottom); cctx.stroke();
  }
  for (let i = 0; i <= 10; i++) {
    const y = bottom - ((bottom - top) * i) / 10;
    cctx.strokeStyle = i % 5 === 0 ? "rgba(43,110,80,.4)" : "rgba(43,110,80,.13)";
    cctx.beginPath(); cctx.moveTo(left, y); cctx.lineTo(right, y); cctx.stroke();
  }

  const maxMs = niceMax(Math.max(100, ...samples.map((s) => s.p95)));
  const maxRps = niceMax(Math.max(20, ...samples.map((s) => s.rps)));
  cctx.fillStyle = "#2b241d";
  cctx.font = "11px 'Courier Prime', monospace";
  cctx.textAlign = "right";
  cctx.fillText(`${maxMs}`, left - 4, top + 8);
  cctx.fillText("0", left - 4, bottom);
  cctx.fillText("ms", left - 4, top + 20);
  cctx.textAlign = "left";
  cctx.fillStyle = "#a3261b";
  cctx.fillText(`${maxRps}`, right + 4, top + 8);
  cctx.fillText("/s", right + 4, top + 20);
  cctx.fillText("0", right + 4, bottom);
  cctx.fillStyle = "#2b241d";
  cctx.textAlign = "center";
  for (let s = 0; s <= 90; s += 30) {
    const x = right - s * PX_PER_SEC;
    if (x > left) cctx.fillText(s ? `-${s}s` : "now", x, h - 4);
  }

  const pen = (key, max, color, width) => {
    cctx.strokeStyle = color;
    cctx.lineWidth = width;
    cctx.beginPath();
    let started = false;
    for (const s of samples) {
      const x = right - ((now - s.t) / 1000) * PX_PER_SEC;
      if (x < left) continue;
      const y = bottom - (Math.min(s[key], max) / max) * (bottom - top);
      started ? cctx.lineTo(x, y) : cctx.moveTo(x, y);
      started = true;
    }
    cctx.stroke();
  };
  cctx.save();
  cctx.beginPath(); cctx.rect(left, top, right - left, bottom - top); cctx.clip();
  pen("rps", maxRps, "rgba(163,38,27,.85)", 1.6);
  pen("p50", maxMs, "#213f6b", 1.8);
  pen("p95", maxMs, "#5e2a6e", 1.8);
  cctx.restore();

  // pen carriage
  cctx.fillStyle = "#2b241d";
  cctx.fillRect(right - 1, top, 2, bottom - top);
}

function niceMax(v) {
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= v) return m * p;
  return 10 * p;
}

// ---------------------------------------------------------------- call log

const tape = $("#tape");
function clock(t) {
  const d = new Date(t);
  return d.toTimeString().slice(0, 8) + "." + String(d.getMilliseconds()).padStart(3, "0").slice(0, 1);
}
function logCalls(batch) {
  // At high rates only a sample fits; failures always make it onto the tape.
  const fails = batch.filter((e) => e.status >= 500 || e.status === 429);
  const rest = batch.filter((e) => !(e.status >= 500 || e.status === 429));
  const pick = [...fails.slice(0, 4), ...rest.slice(0, Math.max(0, 5 - Math.min(fails.length, 4)))];
  for (const ev of pick) {
    const li = document.createElement("li");
    if (ev.status >= 500) li.classList.add("fail");
    if (ev.status === 429) li.classList.add("refused");
    const dest = ev.backend ? `Line ${ev.backend}${ev.attempts > 1 ? "*" : ""}` : "none";
    for (const [cls, text] of [
      ["time", clock(ev.t)],
      ["who", ev.client],
      ["path", `${ev.method} ${ev.path}`],
      ["dest", dest],
      ["code", ev.status],
      ["ms", `${ev.ms < 10 ? ev.ms.toFixed(1) : Math.round(ev.ms)}ms`],
    ]) {
      const span = document.createElement("span");
      span.className = cls;
      span.textContent = text;
      li.appendChild(span);
    }
    tape.prepend(li);
  }
  while (tape.children.length > 40) tape.lastChild.remove();
}

// ---------------------------------------------------------------- controls

const rotary = $("#rotary");
const dial = $(".dial", rotary);
const knob = $("#knob");
const STOP_ANGLES = [-80, -40, 0, 40, 80];
Object.entries(STRATEGIES).forEach(([name, s], i) => {
  const angle = STOP_ANGLES[i];
  const r = 108;
  const x = 145 + r * Math.sin((angle * Math.PI) / 180);
  const y = 104 - r * Math.cos((angle * Math.PI) / 180);
  const label = document.createElement("label");
  label.className = "stop";
  label.style.left = `${x}px`;
  label.style.top = `${y}px`;
  label.innerHTML = `<input type="radio" name="strategy" value="${name}"><span>${s.label}</span>`;
  dial.appendChild(label);
});
rotary.addEventListener("change", (e) => {
  const name = e.target.value;
  setKnob(name);
  act(api("PUT", "strategy", { name }), `Routing set to ${STRATEGIES[name].label.toLowerCase()}.`);
});
function setKnob(name) {
  const i = Object.keys(STRATEGIES).indexOf(name);
  knob.style.setProperty("--angle", `${STOP_ANGLES[i] ?? 0}deg`);
  $("#strategy-note").textContent = STRATEGIES[name]?.note ?? "";
}

const traffic = $("#traffic");
const trafficOut = $("#traffic-out");
const showTraffic = (v) => (trafficOut.textContent = +v ? `${v} calls/s` : "Idle");
let trafficTimer;
traffic.addEventListener("input", () => {
  showTraffic(traffic.value);
  clearTimeout(trafficTimer);
  trafficTimer = setTimeout(() => act(api("PUT", "loadgen", { rps: +traffic.value })), 120);
});

const limit = $("#limit");
const limitOut = $("#limit-out");
const showLimit = (v) => (limitOut.textContent = +v ? `${v} calls/s each` : "Off");
let limitTimer;
limit.addEventListener("input", () => {
  showLimit(limit.value);
  clearTimeout(limitTimer);
  limitTimer = setTimeout(() => act(api("PUT", "ratelimit", { rate: +limit.value, burst: +limit.value * 2 })), 200);
});

function renderControls(s) {
  const checked = rotary.querySelector(`input[value="${s.strategy}"]`);
  if (checked && !checked.checked) checked.checked = true;
  setKnob(s.strategy);

  const wrap = $("#lever-wrap");
  wrap.classList.toggle("disabled", !s.has_loadgen);
  traffic.disabled = !s.has_loadgen;
  traffic.max = s.loadgen_max || 500;
  if (document.activeElement !== traffic) {
    traffic.value = s.loadgen_rps;
    showTraffic(s.loadgen_rps);
  }
  if (!s.has_loadgen) trafficOut.textContent = "Start with -demo";

  if (document.activeElement !== limit) {
    limit.value = s.rate_limit;
    showLimit(s.rate_limit);
  }
  $("#in-demo").hidden = !s.can_install;
}

// ---------------------------------------------------------------- line card

const card = $("#linecard");
let cardId = null;
let chaos = null;

function openLine(id) {
  cardId = id;
  const removeBtn = $("#lc-remove");
  removeBtn.classList.remove("confirm");
  removeBtn.textContent = "Remove line";
  fillCard();
  card.showModal();
  chaos = null;
  $("#lc-faults").hidden = true;
  $("#lc-nofaults").hidden = true;
  fetch(`api/backends/${id}/chaos`)
    .then((r) => (r.ok ? r.json() : Promise.reject()))
    .then((c) => {
      chaos = c;
      fillChaos();
      $("#lc-faults").hidden = false;
    })
    .catch(() => ($("#lc-nofaults").hidden = false));
}

function fillCard() {
  const b = state?.backends.find((x) => x.id === cardId);
  if (!b) {
    if (card.open) card.close();
    return;
  }
  $("#lc-title").textContent = `Line ${b.id}`;
  $("#lc-url").textContent = b.url;
  const status = !b.healthy ? "No answer" : b.breaker !== "closed" ? `Breaker ${b.breaker}` : b.draining ? "Draining" : "In service";
  const rows = [
    ["Status", status], ["In progress", b.active], ["Answered", b.served],
    ["Failed", b.failed], ["p50", `${Math.round(b.p50_ms)} ms`], ["p95", `${Math.round(b.p95_ms)} ms`],
    ["p99", `${Math.round(b.p99_ms)} ms`], ["Smoothed", `${Math.round(b.ewma_ms)} ms`], ["Weight", b.weight],
  ];
  $("#lc-stats").innerHTML = rows.map(([k, v]) => `<div><dt>${k}</dt><dd>${v}</dd></div>`).join("");
  $("#lc-weight").textContent = b.weight;
  if (document.activeElement !== $("#lc-drain")) $("#lc-drain").checked = b.draining;
}

function fillChaos() {
  $("#lc-down").checked = chaos.down;
  $("#lc-lat").value = chaos.latency_ms;
  $("#lc-jit").value = chaos.jitter_ms;
  $("#lc-err").value = Math.round(chaos.error_rate * 100);
  showChaos();
}
function showChaos() {
  $("#lc-lat-out").textContent = `${$("#lc-lat").value} ms`;
  $("#lc-jit-out").textContent = `up to ${$("#lc-jit").value} ms`;
  $("#lc-err-out").textContent = `${$("#lc-err").value}%`;
}

function setWeight(delta) {
  const b = state.backends.find((x) => x.id === cardId);
  const weight = Math.max(0, Math.min(100, b.weight + delta));
  act(api("PATCH", `backends/${cardId}`, { weight }));
}
$("#lc-wdown").addEventListener("click", () => setWeight(-1));
$("#lc-wup").addEventListener("click", () => setWeight(1));
$("#lc-drain").addEventListener("change", (e) => {
  act(api("PATCH", `backends/${cardId}`, { draining: e.target.checked }), e.target.checked ? `Line ${cardId} is draining.` : `Line ${cardId} is back in service.`);
});

let chaosTimer;
function pushChaos() {
  const body = {
    down: $("#lc-down").checked,
    latency_ms: +$("#lc-lat").value,
    jitter_ms: +$("#lc-jit").value,
    error_rate: +$("#lc-err").value / 100,
  };
  clearTimeout(chaosTimer);
  chaosTimer = setTimeout(() => act(api("PUT", `backends/${cardId}/chaos`, body)), 150);
}
for (const id of ["#lc-lat", "#lc-jit", "#lc-err"]) {
  $(id).addEventListener("input", () => { showChaos(); pushChaos(); });
}
$("#lc-down").addEventListener("change", (e) => {
  pushChaos();
  notify(e.target.checked ? `Pulled the plug on line ${cardId}.` : `Line ${cardId} plugged back in.`);
});

$("#lc-remove").addEventListener("click", (e) => {
  const btn = e.currentTarget;
  if (!btn.classList.contains("confirm")) {
    btn.classList.add("confirm");
    btn.textContent = `Remove line ${cardId}? Calls in progress will finish.`;
    return;
  }
  const id = cardId;
  card.close();
  act(api("DELETE", `backends/${id}`), `Line ${id} removed.`);
});

// ---------------------------------------------------------------- installer

const installer = $("#installer");
$("#install").addEventListener("click", () => {
  $("#in-url").value = "";
  installer.showModal();
});
$("#in-close").addEventListener("click", () => installer.close());
$("#install-form").addEventListener("submit", (e) => {
  e.preventDefault();
  const url = $("#in-url").value.trim();
  const weight = +$("#in-weight").value || 1;
  installer.close();
  act(api("POST", "backends", { url, weight }), `Installed ${url}.`);
});
$("#in-demo").addEventListener("click", () => {
  installer.close();
  act(api("POST", "backends", { demo: true }), "Installed a demo line.");
});

// ---------------------------------------------------------------- live feed

const exchange = $("#exchange");
function setOnline(on) {
  exchange.classList.toggle("offline", !on);
  if (!on) exchange.textContent = "Exchange offline, reconnecting";
}

let lampTested = false;
function lampTest() {
  lampTested = true;
  if (reduceMotion) return;
  [...lineEls.values()].forEach((l, i) => {
    l.lamp.animate(
      [{ background: "#ffe2a0", boxShadow: "0 0 0 3px #8c6b2a, 0 0 0 5px #120e0c, 0 0 30px 10px rgba(255,181,71,.8)" }, {}],
      { duration: 500, delay: 120 * i, easing: "ease-out" },
    );
  });
  callerEls.forEach((c, i) => c.animate([{ filter: "brightness(2)" }, {}], { duration: 300, delay: 300 + 18 * i }));
}

function applyState(s) {
  state = s;
  const up = s.backends.filter((b) => lineState(b) === "ok").length;
  exchange.classList.remove("offline");
  exchange.textContent = `${location.host}, ${up} of ${s.backends.length} lines in service, ${s.requests.toLocaleString()} calls handled`;
  renderLines(s.backends);
  renderControls(s);
  if (!lampTested) lampTest();
  if (card.open) fillCard();
}

function onCalls(batch) {
  const win = recentWindow(3000).filter((e) => e.backend).map((e) => e.ms).sort((a, b) => a - b);
  const slowMs = Math.max(250, quantile(win, 0.9));
  batch.forEach((ev, i) => {
    recent.push(ev);
    if (!reduceMotion) addCord(ev, (i / batch.length) * 100, slowMs);
  });
  logCalls(batch);
}

const events = new EventSource("api/events");
events.addEventListener("state", (e) => applyState(JSON.parse(e.data)));
events.addEventListener("calls", (e) => onCalls(JSON.parse(e.data)));
events.addEventListener("open", () => exchange.classList.remove("offline"));
events.addEventListener("error", () => setOnline(false));

// ---------------------------------------------------------------- frame loop

let lastFrame = performance.now();
function frame(now) {
  const dt = Math.min(0.05, (now - lastFrame) / 1000);
  lastFrame = now;
  if (layoutDirty) layout();

  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, canvas.width, canvas.height);
  for (let i = cords.length - 1; i >= 0; i--) {
    if (drawCord(cords[i], now)) {
      callerLoad[cords[i].from]--;
      cords.splice(i, 1);
    }
  }
  callerEls.forEach((el, i) => el.classList.toggle("on", callerLoad[i] > 0));

  const win = recentWindow(3000);
  const answered = win.filter((e) => e.backend && e.status !== 429).map((e) => e.ms).sort((a, b) => a - b);
  meters.rps.set(state ? state.rps[state.rps.length - 1] || 0 : 0);
  meters.p95.set(quantile(answered, 0.95));
  meters.err.set(win.length ? (100 * win.filter((e) => e.status >= 500).length) / win.length : 0);
  for (const m of Object.values(meters)) m.step(dt);

  if (now - lastSample >= 500) {
    lastSample = now;
    sampleChart();
  }
  drawChart();
  requestAnimationFrame(frame);
}
requestAnimationFrame(frame);

