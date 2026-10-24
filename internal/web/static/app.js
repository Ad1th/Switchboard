// Switchboard status page. State arrives over server-sent events: a snapshot
// twice a second and batches of finished requests every 100 ms.

const $ = (sel) => document.querySelector(sel);

const STRATEGIES = {
  roundrobin: "Round robin",
  weighted: "Weighted round robin",
  leastconn: "Least connections",
  p2c: "Power of two choices",
  hash: "Consistent hash (sticky by client)",
};

// Same rule as demo.Color in Go, so a backend's row matches its page.
const PALETTE = ["#1f77b4", "#d62728", "#2ca02c", "#9467bd", "#ff7f0e", "#17becf", "#8c564b", "#e377c2"];
function color(name) {
  const m = /(\d+)$/.exec(name);
  if (m && +m[1] > 0) return PALETTE[(+m[1] - 1) % PALETTE.length];
  let h = 2166136261;
  for (const ch of new TextEncoder().encode(name)) h = Math.imul(h ^ ch, 16777619) >>> 0;
  return PALETTE[h % PALETTE.length];
}

let state = null;
const recent = []; // finished requests, newest last

function showError(msg) {
  $("#error").textContent = msg;
  clearTimeout(showError.t);
  showError.t = setTimeout(() => ($("#error").textContent = ""), 5000);
}

async function api(method, path, body) {
  const res = await fetch(`api/${path}`, {
    method,
    headers: { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `${res.status} ${res.statusText}`);
  if (data.backends) render(data);
  return data;
}
const run = (p) => p.catch((e) => showError(e.message));

// ---------------------------------------------------------------- controls

const strategySel = $("#strategy");
for (const [value, label] of Object.entries(STRATEGIES)) strategySel.add(new Option(label, value));
strategySel.addEventListener("change", () => run(api("PUT", "strategy", { name: strategySel.value })));

$("#traffic-form").addEventListener("submit", (e) => {
  e.preventDefault();
  run(api("PUT", "loadgen", { rps: +$("#traffic").value }));
});
$("#limit-form").addEventListener("submit", (e) => {
  e.preventDefault();
  const rate = +$("#limit").value;
  run(api("PUT", "ratelimit", { rate, burst: rate * 2 }));
});
$("#add-form").addEventListener("submit", (e) => {
  e.preventDefault();
  run(api("POST", "backends", { url: $("#add-url").value.trim(), name: $("#add-name").value.trim() }).then(() => {
    $("#add-url").value = "";
    $("#add-name").value = "";
  }));
});
$("#add-demo").addEventListener("click", () => run(api("POST", "backends", { demo: true })));

$("#probe").addEventListener("click", async () => {
  const out = $("#probe-out");
  const t0 = performance.now();
  try {
    const res = await fetch("/", { headers: { Accept: "text/plain" }, cache: "no-store" });
    const text = (await res.text()).trim();
    const ms = Math.round(performance.now() - t0);
    const name = res.headers.get("X-Backend-Name");
    out.textContent = `${res.status}, ${ms} ms: ${name ? `answered by ${name}` : text}`;
    out.style.color = name ? color(name) : "";
  } catch (e) {
    out.textContent = e.message;
  }
});

// ---------------------------------------------------------------- backends table

const tbody = $("#backends");
const rows = new Map(); // id -> { tr, faultsTr, cells }
const openFaults = new Set();

function status(b) {
  if (!b.healthy) return ["down", "DOWN"];
  if (b.breaker === "open") return ["open", "OPEN (breaker)"];
  if (b.breaker === "half-open") return ["half", "HALF-OPEN"];
  if (b.draining) return ["drain", "DRAINING"];
  if (b.weight === 0) return ["drain", "WEIGHT 0"];
  return ["up", "UP"];
}

function ms(v) {
  return v ? (v < 10 ? v.toFixed(1) : Math.round(v)) : "-";
}

function el(tag, attrs = {}, ...kids) {
  const n = document.createElement(tag);
  Object.assign(n, attrs);
  n.append(...kids);
  return n;
}

function makeRow(b) {
  const tr = el("tr");
  const c = {};
  for (const k of ["name", "addr", "status", "weight", "active", "served", "failed", "err", "p50", "p95", "p99", "share", "actions"]) {
    c[k] = el("td");
    if (["active", "served", "failed", "err", "p50", "p95", "p99"].includes(k)) c[k].className = "num";
    tr.append(c[k]);
  }
  c.swatch = el("span", { className: "swatch" });
  c.nameText = document.createTextNode("");
  c.name.append(c.swatch, c.nameText);
  c.link = el("a", { target: "_blank", rel: "noopener", title: "Open this backend directly, bypassing the balancer" });
  c.addr.append(c.link);
  c.statusText = el("span", { className: "status" });
  c.status.append(c.statusText);

  c.weightIn = el("input", { type: "number", min: 0, max: 100, className: "weight", title: "Weight (used by weighted, least connections, two choices and hash)" });
  c.weightIn.addEventListener("change", () => run(api("PATCH", `backends/${b.id}`, { weight: +c.weightIn.value })));
  c.weight.append(c.weightIn);

  c.fill = el("div", { className: "fill" });
  c.sharePct = el("span");
  c.share.append(el("div", { className: "share" }, el("div", { className: "track" }, c.fill), c.sharePct));

  c.drainBtn = el("button", { type: "button" });
  c.drainBtn.addEventListener("click", () => {
    const b2 = state.backends.find((x) => x.id === b.id);
    run(api("PATCH", `backends/${b.id}`, { draining: !b2.draining }));
  });
  const faultsBtn = el("button", { type: "button", textContent: "Faults" });
  faultsBtn.addEventListener("click", () => toggleFaults(b.id));
  const removeBtn = el("button", { type: "button", textContent: "Remove" });
  removeBtn.addEventListener("click", () => {
    if (confirm(`Remove ${state.backends.find((x) => x.id === b.id)?.name}? Requests in progress will finish.`)) {
      run(api("DELETE", `backends/${b.id}`));
    }
  });
  c.actions.append(el("div", { className: "actions" }, c.drainBtn, faultsBtn, removeBtn));
  tbody.append(tr);
  return { tr, c };
}

function toggleFaults(id) {
  const row = rows.get(id);
  if (row.faultsTr) {
    row.faultsTr.remove();
    row.faultsTr = null;
    openFaults.delete(id);
    return;
  }
  openFaults.add(id);
  const td = el("td", { colSpan: 13 });
  const down = el("input", { type: "checkbox" });
  const lat = el("input", { type: "number", min: 0, max: 10000, step: 10 });
  const jit = el("input", { type: "number", min: 0, max: 10000, step: 10 });
  const err = el("input", { type: "number", min: 0, max: 100, step: 5 });
  const apply = el("button", { textContent: "Apply" });
  const form = el("form", { className: "bar" },
    el("strong", { textContent: "Inject faults:" }),
    el("label", {}, down, "Down (fail health checks, drop connections)"),
    el("label", {}, "Added latency", lat, "ms"),
    el("label", {}, "Jitter", jit, "ms"),
    el("label", {}, "Error rate", err, "%"),
    apply,
  );
  form.addEventListener("submit", (e) => {
    e.preventDefault();
    run(api("PUT", `backends/${id}/chaos`, {
      down: down.checked, latency_ms: +lat.value, jitter_ms: +jit.value, error_rate: +err.value / 100,
    }));
  });
  td.append(form);
  row.faultsTr = el("tr", { className: "faults" }, td);
  row.tr.after(row.faultsTr);
  fetch(`api/backends/${id}/chaos`)
    .then((r) => (r.ok ? r.json() : r.json().then((d) => Promise.reject(new Error(d.error)))))
    .then((c) => {
      down.checked = c.down;
      lat.value = c.latency_ms;
      jit.value = c.jitter_ms;
      err.value = Math.round(c.error_rate * 100);
    })
    .catch((e) => form.replaceChildren(el("span", { className: "hint", textContent: e.message })));
}

function renderBackends(backends) {
  const shares = shareByBackend(10000);
  const total = Object.values(shares).reduce((a, b) => a + b, 0);
  const seen = new Set();
  for (const b of backends) {
    seen.add(b.id);
    let row = rows.get(b.id);
    if (!row) {
      row = makeRow(b);
      rows.set(b.id, row);
    }
    const c = row.c;
    const [cls, label] = status(b);
    row.tr.className = cls;
    c.swatch.style.background = color(b.name);
    c.nameText.data = b.name;
    c.link.href = b.url;
    c.link.textContent = b.url.replace(/^https?:\/\//, "");
    c.statusText.className = `status ${cls}`;
    c.statusText.textContent = label;
    if (document.activeElement !== c.weightIn) c.weightIn.value = b.weight;
    c.active.textContent = b.active;
    c.served.textContent = b.served.toLocaleString();
    c.failed.textContent = b.failed.toLocaleString();
    c.err.textContent = b.served ? ((100 * b.failed) / b.served).toFixed(1) : "-";
    c.p50.textContent = ms(b.p50_ms);
    c.p95.textContent = ms(b.p95_ms);
    c.p99.textContent = ms(b.p99_ms);
    const pct = total ? (100 * (shares[b.id] || 0)) / total : 0;
    c.fill.style.width = `${pct}%`;
    c.fill.style.background = color(b.name);
    c.sharePct.textContent = `${Math.round(pct)}%`;
    c.drainBtn.textContent = b.draining ? "Undrain" : "Drain";
  }
  for (const [id, row] of rows) {
    if (!seen.has(id)) {
      row.tr.remove();
      row.faultsTr?.remove();
      rows.delete(id);
    }
  }
}

function shareByBackend(windowMs) {
  const cutoff = Date.now() - windowMs;
  const out = {};
  for (let i = recent.length - 1; i >= 0 && recent[i].t >= cutoff; i--) {
    const b = recent[i].backend;
    if (b) out[b] = (out[b] || 0) + 1;
  }
  return out;
}

// ---------------------------------------------------------------- summary

function quantile(sorted, q) {
  return sorted.length ? sorted[Math.min(sorted.length - 1, Math.floor(q * sorted.length))] : 0;
}

function duration(s) {
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), sec = Math.floor(s % 60);
  return h ? `${h}h ${m}m` : m ? `${m}m ${sec}s` : `${sec}s`;
}

function renderSummary(s) {
  const cutoff = Date.now() - 5000;
  const lat = recent.filter((e) => e.t >= cutoff && e.backend && e.status < 500).map((e) => e.ms).sort((a, b) => a - b);
  const cells = [
    s.rps[s.rps.length - 1] ?? 0,
    s.requests.toLocaleString(),
    s.errors.toLocaleString(),
    s.limited.toLocaleString(),
    s.retries.toLocaleString(),
    lat.length ? `${ms(quantile(lat, 0.5))} ms` : "-",
    lat.length ? `${ms(quantile(lat, 0.95))} ms` : "-",
    duration(s.uptime_s),
  ];
  $("#summary").replaceChildren(...cells.map((v) => el("td", { textContent: v })));
  const up = s.backends.filter((b) => status(b)[0] === "up").length;
  $("#meta").textContent = `${location.host}, ${up}/${s.backends.length} backends up, strategy ${STRATEGIES[s.strategy] || s.strategy}`;
}

function renderControls(s) {
  if (document.activeElement !== strategySel) strategySel.value = s.strategy;
  $("#traffic-form").hidden = !s.has_loadgen;
  if (document.activeElement !== $("#traffic")) $("#traffic").value = s.loadgen_rps;
  if (document.activeElement !== $("#limit")) $("#limit").value = s.rate_limit;
  $("#add-demo").hidden = !s.can_install;
}

// ---------------------------------------------------------------- charts

const SECS = 60;

function perSecond() {
  // Bucket finished requests by second, per backend.
  const nowSec = Math.floor(Date.now() / 1000);
  const series = {};
  const lat = Array.from({ length: SECS }, () => []);
  for (const e of recent) {
    const i = SECS - 1 - (nowSec - 1 - Math.floor(e.t / 1000));
    if (i < 0 || i >= SECS) continue;
    if (e.backend) {
      (series[e.backend] ||= new Array(SECS).fill(0))[i]++;
      if (e.status < 500) lat[i].push(e.ms);
    }
  }
  return { series, lat };
}

function polyline(values, max, stroke) {
  const pts = values.map((v, i) => `${(i / (SECS - 1)) * 600},${180 - (Math.min(v, max) / max) * 172 - 4}`).join(" ");
  const p = document.createElementNS("http://www.w3.org/2000/svg", "polyline");
  p.setAttribute("points", pts);
  p.setAttribute("stroke", stroke);
  return p;
}

function grid(svg) {
  svg.replaceChildren();
  for (let i = 1; i < 4; i++) {
    const y = (180 / 4) * i;
    const l = document.createElementNS("http://www.w3.org/2000/svg", "line");
    Object.entries({ x1: 0, x2: 600, y1: y, y2: y, class: "grid" }).forEach(([k, v]) => l.setAttribute(k, v));
    svg.append(l);
  }
}

function niceMax(v) {
  const p = Math.pow(10, Math.floor(Math.log10(Math.max(v, 1))));
  for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= v) return m * p;
  return 10 * p;
}

function renderCharts() {
  if (!state) return;
  const { series, lat } = perSecond();
  const svg = $("#chart-rps");
  grid(svg);
  const max = niceMax(Math.max(5, ...Object.values(series).flat()));
  const legend = [];
  for (const b of state.backends) {
    const vals = series[b.id] || new Array(SECS).fill(0);
    svg.append(polyline(vals, max, color(b.name)));
    legend.push(el("span", {}, el("i", { style: `background:${color(b.name)}` }), b.name));
  }
  legend.push(el("span", { textContent: `scale 0 to ${max} req/s` }));
  $("#legend").replaceChildren(...legend);

  const p50 = lat.map((a) => quantile(a.sort((x, y) => x - y), 0.5));
  const p95 = lat.map((a) => quantile(a, 0.95));
  const lsvg = $("#chart-lat");
  grid(lsvg);
  const lmax = niceMax(Math.max(50, ...p95));
  const css = getComputedStyle(document.documentElement);
  lsvg.append(polyline(p50, lmax, css.getPropertyValue("--p50").trim()), polyline(p95, lmax, css.getPropertyValue("--p95").trim()));
  $("#lat-scale").textContent = `scale 0 to ${lmax} ms`;
}

// ---------------------------------------------------------------- request log

const logBody = $("#log");
function logRequests(batch) {
  const errors = batch.filter((e) => e.status >= 500 || e.status === 429);
  const ok = batch.filter((e) => !(e.status >= 500 || e.status === 429));
  const pick = [...errors.slice(0, 4), ...ok.slice(0, Math.max(1, 4 - errors.length))];
  const names = Object.fromEntries((state?.backends || []).map((b) => [b.id, b.name]));
  for (const e of pick) {
    const d = new Date(e.t);
    const name = names[e.backend] || (e.backend ? `#${e.backend}` : "none");
    const tr = el("tr", { className: e.status >= 500 ? "err" : e.status === 429 ? "limited" : "" },
      el("td", { textContent: d.toTimeString().slice(0, 8) + "." + String(d.getMilliseconds()).padStart(3, "0") }),
      el("td", { textContent: e.client }),
      el("td", { className: "req", textContent: `${e.method} ${e.path}`, title: `${e.method} ${e.path}` }),
      el("td", {}, el("span", { className: "swatch", style: `background:${e.backend ? color(name) : "transparent"}` }), name),
      el("td", { className: "num", textContent: e.status }),
      el("td", { className: "num", textContent: ms(e.ms) }),
      el("td", { className: "num", textContent: e.attempts }),
    );
    logBody.prepend(tr);
  }
  while (logBody.children.length > 30) logBody.lastChild.remove();
}

// ---------------------------------------------------------------- live feed

function render(s) {
  state = s;
  renderControls(s);
  renderSummary(s);
  renderBackends(s.backends);
}

const events = new EventSource("api/events");
events.addEventListener("state", (e) => render(JSON.parse(e.data)));
events.addEventListener("calls", (e) => {
  const batch = JSON.parse(e.data);
  recent.push(...batch);
  const cutoff = Date.now() - (SECS + 2) * 1000;
  let drop = 0;
  while (drop < recent.length && recent[drop].t < cutoff) drop++;
  if (drop) recent.splice(0, drop);
  logRequests(batch);
});
events.addEventListener("error", () => ($("#meta").textContent = "connection lost, retrying…"));

setInterval(renderCharts, 1000);
