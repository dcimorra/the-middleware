// What a site moves over the network: bytes in and out as a rate now, the
// last minute drawn second by second, today by the hour, the days of a
// period, and its budget with where it stands in it. The house counts as
// it serves (GET /sites/{d}/network); this tab reads totals and draws.
//
// The top card is asked for every two seconds while it is on the page,
// since a rate that is ten seconds old is not "now"; the charts under it
// are drawn again only when their numbers changed, or a column under the
// pointer would lose its hover every two seconds.

import { h, svg, clear, fmtBytes } from "../lib/dom.js";
import { t, currentLang } from "../lib/i18n.js";
import { api, enc, qs } from "../lib/api.js";
import { errorBox, loading, notice } from "../components/page.js";
import { toggle } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { delegate } from "../lib/access.js";
import { parseSize } from "./site-resources.js";

const UNITS = { m: 1 << 20, g: 1 << 30, t: 1024 * (1 << 30) };
const rate = (n) => fmtBytes(Math.round(n || 0)) + "/s";
const both = (x) => (x ? (x.in || 0) + (x.out || 0) : 0);

export function networkTab(domain) {
  const period = h("select", { "aria-label": t("net.period") },
    ...[7, 30, 90].map((n) => h("option", { value: String(n) }, t("visits.lastDays", { n }))));
  period.value = "30";
  const live = h("div", null, loading());
  const charts = h("div", { class: "card" });
  const budget = h("div");
  const el = h("div", { class: "cards" },
    h("div", { class: "card" },
      h("div", { class: "log-bar" }, h("h2", { class: "grow" }, t("net.title")), period),
      h("p", { class: "small muted" }, t("net.hint")),
      live),
    charts, budget);
  if (!delegate()) el.append(budgetCard(domain, () => load(true)));

  let drawn = "";
  let seen = false;

  function tile(label, value, hint) {
    return h("div", { class: "stat", title: hint || "" }, h("div", { class: "label" }, label), h("div", { class: "value" }, value), hint ? h("div", { class: "small muted" }, hint) : null);
  }
  const split = (x) => t("net.split", { in: fmtBytes(x.in || 0), out: fmtBytes(x.out || 0) });

  function drawLive(rep) {
    const state = rep.state || "ok";
    const ports = both({ in: rep.month.ports_in, out: rep.month.ports_out });
    clear(live).append(...[
      state === "cut" ? notice("bad", t("net.state.cut")) : null,
      state === "over" ? notice("warn", t("net.state.over")) : null,
      state === "warn" ? notice("warn", t("net.state.warn", { n: (rep.limits || {}).warn_at || 80 })) : null,
      h("div", { class: "stats site-stats" },
        tile(t("net.inNow"), rate(rep.rate_in)),
        tile(t("net.outNow"), rate(rep.rate_out)),
        tile(t("net.today"), fmtBytes(both(rep.today)), split(rep.today)),
        tile(t("net.month"), fmtBytes(both(rep.month)), split(rep.month))),
      h("h3", null, t("net.lastMinute")),
      spark(rep.seconds_in || [], rep.seconds_out || []),
      ports ? h("p", { class: "small muted" }, t("net.ports", { n: fmtBytes(ports) })) : null,
    ].filter(Boolean));
  }

  function drawBudget(rep) {
    clear(budget);
    const l = rep.limits;
    if (!l) return;
    const row = (label, used, limit) => {
      const f = limit ? used / limit : 0;
      return h("div", { class: "net-budget" },
        h("div", { class: "site-res-head" }, h("span", null, label),
          h("strong", { class: f >= 1 ? "level-bad" : f * 100 >= l.warn_at ? "level-warn" : "" }, t("net.usedOf", { used: fmtBytes(used), limit: fmtBytes(limit), pct: Math.floor(f * 100) }))),
        meter(f, l.warn_at / 100));
    };
    budget.append(h("div", { class: "card" },
      h("h3", null, t("net.budget")),
      l.monthly ? row(t("net.monthly"), both(rep.month), l.monthly) : null,
      l.daily ? row(t("net.daily"), both(rep.today), l.daily) : null,
      h("p", { class: "small muted" }, t(l.cut ? "net.thenCut" : "net.thenWarn", { n: l.warn_at }))));
  }

  function drawCharts(rep) {
    const days = rep.days || [];
    const key = JSON.stringify([days, rep.hours_in, rep.hours_out]);
    if (key === drawn) return;
    drawn = key;
    const day = (d) => new Date(d + "T12:00").toLocaleDateString(currentLang(), { weekday: "short", day: "numeric", month: "short" });
    const short = (d) => new Date(d + "T12:00").toLocaleDateString(currentLang(), { day: "numeric", month: "short" });
    clear(charts).append(
      h("div", { class: "log-bar" }, h("h3", { class: "grow" }, t("net.byDay")),
        h("span", { class: "small muted" }, t("net.periodTotal", { n: fmtBytes(both(rep.period)), in: fmtBytes(rep.period.in || 0), out: fmtBytes(rep.period.out || 0) }))),
      legend(),
      bars(days.map((d) => ({ in: d.in, out: d.out, label: t("net.bar", { when: day(d.date), in: fmtBytes(d.in), out: fmtBytes(d.out) }) })),
        days.length ? [short(days[0].date), short(days[days.length - 1].date)] : []),
      h("h3", null, t("net.byHour")),
      bars((rep.hours_in || []).map((n, i) => ({ in: n, out: (rep.hours_out || [])[i] || 0, label: t("net.bar", { when: String(i).padStart(2, "0") + " h", in: fmtBytes(n), out: fmtBytes((rep.hours_out || [])[i] || 0) }) })), ["00", "12", "23"]));
  }

  async function load(force) {
    let rep;
    try { rep = await api.get(`/sites/${enc(domain)}/network` + qs({ days: period.value })); } catch (e) {
      if (!live.querySelector(".stats")) clear(live).append(errorBox(e, () => load(true)));
      return;
    }
    if (force) drawn = "";
    drawLive(rep);
    drawBudget(rep);
    drawCharts(rep);
  }

  period.addEventListener("change", () => load(true));
  // Polled while on the page, as the resources card is: the site's page
  // takes it away when it closes.
  const tick = async () => {
    if (el.isConnected) seen = true;
    else if (seen) return;
    if (!document.hidden) await load(false);
    setTimeout(tick, 2000);
  };
  tick();
  return { el };
}

function legend() {
  return h("div", { class: "net-legend small muted" },
    h("span", { class: "net-key in" }), t("net.in"), h("span", { class: "net-key out" }), t("net.out"));
}

// spark is the last minute as two lines, in and out, on one scale.
// Attributes only, no style: the CSP drops inline ones.
function spark(ins, outs) {
  const W = 600, H = 70, n = Math.max(ins.length, outs.length, 2);
  const max = Math.max(1, ...ins, ...outs);
  const line = (vals) => vals.map((v, i) => `${((i / (n - 1)) * W).toFixed(1)},${(H - 2 - ((v || 0) / max) * (H - 6)).toFixed(1)}`).join(" ");
  return h("div", { class: "visits-chart" },
    h("span", { class: "peak" }, t("net.peak", { n: rate(max) })),
    svg("svg", { class: "net-spark", viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: "none", role: "img", "aria-label": t("net.lastMinute") },
      svg("line", { class: "base", x1: 0, x2: W, y1: H - 0.5, y2: H - 0.5 }),
      svg("polyline", { class: "out", points: line(outs) }),
      svg("polyline", { class: "in", points: line(ins) })),
    legend());
}

// bars is a column per item, what went out with what came in on top of
// it, its line on hover, the first and last labels under it.
function bars(items, ticks) {
  const W = 600, H = 120, gap = items.length > 60 ? 1 : 2;
  const max = Math.max(1, ...items.map((i) => (i.in || 0) + (i.out || 0)));
  const w = W / Math.max(1, items.length);
  const chart = svg("svg", { class: "visits-bars net-bars", viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: "none", role: "img", "aria-label": t("net.peakTotal", { n: fmtBytes(max) }) });
  chart.append(svg("line", { class: "base", x1: 0, x2: W, y1: H - 0.5, y2: H - 0.5 }));
  items.forEach((it, i) => {
    const total = (it.in || 0) + (it.out || 0);
    const bh = total ? Math.max(2, (total / max) * (H - 4)) : 0;
    const oh = total ? bh * ((it.out || 0) / total) : 0;
    const x = (i * w + gap / 2).toFixed(2), bw = Math.max(1, w - gap).toFixed(2);
    chart.append(svg("g", null, svg("title", null, it.label),
      svg("rect", { class: "hit", x: (i * w).toFixed(2), y: 0, width: w.toFixed(2), height: H }),
      svg("rect", { class: "bar out", x, y: (H - oh).toFixed(2), width: bw, height: oh.toFixed(2) }),
      svg("rect", { class: "bar in", x, y: (H - bh).toFixed(2), width: bw, height: (bh - oh).toFixed(2) })));
  });
  return h("div", { class: "visits-chart" }, h("span", { class: "peak" }, t("net.peakTotal", { n: fmtBytes(max) })), chart,
    h("div", { class: "visits-ticks small muted" }, ...ticks.map((x) => h("span", null, x))));
}

// meter is how far into a budget, with the warning share marked on it.
function meter(f, warn) {
  const W = 600;
  const cls = f >= 1 ? "bad" : f >= warn ? "warn" : "ok";
  return svg("svg", { class: "net-meter " + cls, viewBox: `0 0 ${W} 10`, preserveAspectRatio: "none", "aria-hidden": "true" },
    svg("rect", { class: "track", x: 0, y: 0, width: W, height: 10, rx: 5 }),
    svg("rect", { class: "fill", x: 0, y: 0, width: (Math.max(0, Math.min(1, f)) * W).toFixed(1), height: 10, rx: 5 }),
    svg("rect", { class: "mark", x: (warn * W - 1).toFixed(1), y: 0, width: 2, height: 10 }));
}

// budgetCard writes the manifest's limits.network, the owner's alone as
// the rest of limits is: the block this card does not draw (memory, CPU)
// is kept as it was read.
function budgetCard(domain, saved) {
  const el = h("section", { class: "card" }, h("h3", null, t("net.setTitle")), loading());
  const size = (label, hint) => {
    const num = h("input", { type: "number", min: "0", step: "any", inputmode: "decimal", placeholder: t("net.none"), "aria-label": label });
    const unit = h("select", { "aria-label": t("net.unit") }, h("option", { value: "m" }, "MB"), h("option", { value: "g" }, "GB"), h("option", { value: "t" }, "TB"));
    unit.value = "g";
    return {
      el: h("div", { class: "field" }, h("label", null, label), h("div", { class: "net-size" }, num, unit), h("div", { class: "hint" }, hint)),
      set(text) {
        const n = parseSize(text);
        if (!n) { num.value = ""; unit.value = "g"; return; }
        const u = n >= UNITS.t && n % UNITS.g === 0 && n % UNITS.t === 0 ? "t" : n >= UNITS.g ? "g" : "m";
        unit.value = u;
        num.value = String(Math.round((n / UNITS[u]) * 100) / 100);
      },
      get() { const n = Number(num.value); return n > 0 ? `${n}${unit.value}` : ""; },
    };
  };
  const monthly = size(t("net.monthly"), t("net.monthlyHint"));
  const daily = size(t("net.daily"), t("net.dailyHint"));
  const warn = h("input", { type: "number", min: "1", max: "99", step: "1", placeholder: "80", "aria-label": t("net.warnAt") });
  const cut = toggle(t("net.cut"));
  const save = h("button", { type: "button", class: "btn primary" }, t("common.save"));
  let limits = null;

  async function load() {
    let data;
    try { data = await api.get(`/sites/${enc(domain)}/limits`); } catch (e) { clear(el).append(h("h3", null, t("net.setTitle")), errorBox(e, load)); return; }
    limits = data.limits || null;
    const n = (limits && limits.network) || {};
    monthly.set(n.monthly);
    daily.set(n.daily);
    warn.value = n.warn_at ? String(n.warn_at) : "";
    cut.input.checked = !!n.cut;
    const def = data.defaults && data.defaults.network;
    clear(el).append(...[
      h("h3", null, t("net.setTitle")),
      h("p", { class: "small muted" }, t("net.setHint")),
      def && !(limits && limits.network) ? notice("info", t("net.fromHouse")) : null,
      h("div", { class: "form-row" }, monthly.el, daily.el,
        h("div", { class: "field" }, h("label", null, t("net.warnAt")), warn, h("div", { class: "hint" }, t("net.warnAtHint")))),
      cut.el,
      h("p", { class: "small muted" }, t("net.cutHint")),
      h("div", { class: "form-actions" }, save)].filter(Boolean));
  }

  save.addEventListener("click", async () => {
    const n = {};
    if (monthly.get()) n.monthly = monthly.get();
    if (daily.get()) n.daily = daily.get();
    if (Number(warn.value) > 0) n.warn_at = Math.round(Number(warn.value));
    if (cut.input.checked) n.cut = true;
    const out = { ...(limits || {}) };
    delete out.network;
    if (n.monthly || n.daily) out.network = n;
    save.disabled = true;
    try {
      await api.patch(`/sites/${enc(domain)}/manifest`, { limits: Object.keys(out).length ? out : null });
      toast(t("net.saved"), "ok");
      await load();
      saved();
    } catch (e) { toastError(e); }
    save.disabled = false;
  });
  load();
  return el;
}
