// A site's audience: people per day and per month, from where, on what,
// to which pages. Counted by the house as pages are served, robots left
// out, so this tab only reads totals: nothing is computed here but sums
// and shares.

import { h, svg, clear } from "../lib/dom.js";
import { t, currentLang } from "../lib/i18n.js";
import { api, enc, qs } from "../lib/api.js";
import { table, cell, empty, errorBox, loading } from "../components/page.js";

const BROWSERS = { chrome: "Chrome", safari: "Safari", firefox: "Firefox", edge: "Edge", opera: "Opera", samsung: "Samsung Internet", ie: "Internet Explorer" };
const SYSTEMS = { windows: "Windows", macos: "macOS", ios: "iOS", android: "Android", linux: "Linux", chromeos: "ChromeOS" };

export function visitsTab(domain) {
  const period = h("select", { "aria-label": t("visits.period") });
  const body = h("div", null, loading());
  const el = h("div", { class: "cards" },
    h("div", { class: "card" },
      h("div", { class: "log-bar" }, h("h2", { class: "grow" }, t("visits.title")), period),
      h("p", { class: "small muted" }, t("visits.hint"))),
    body);

  let names = null;
  try { names = new Intl.DisplayNames([currentLang()], { type: "region" }); } catch { names = null; }
  const country = (code) => { try { return (names && names.of(code)) || code; } catch { return code; } };
  const monthLabel = (m) => {
    const [y, mo] = m.split("-").map(Number);
    return new Date(y, mo - 1, 1).toLocaleDateString(currentLang(), { month: "long", year: "numeric" });
  };

  // The months come with every answer; the list is rebuilt keeping what
  // was picked.
  function fillPeriods(months) {
    const keep = period.value || "days:30";
    clear(period).append(
      ...[7, 30, 90].map((n) => h("option", { value: "days:" + n }, t("visits.lastDays", { n }))),
      ...months.map((m) => h("option", { value: "month:" + m.month }, monthLabel(m.month))));
    period.value = [...period.options].some((o) => o.value === keep) ? keep : "days:30";
  }

  async function load() {
    const [kind, val] = (period.value || "days:30").split(":");
    let rep;
    try {
      rep = await api.get(`/sites/${enc(domain)}/visits` + qs(kind === "month" ? { month: val } : { days: val }));
    } catch (e) { clear(body).append(h("div", { class: "card" }, errorBox(e, load))); return; }
    fillPeriods(rep.months || []);
    clear(body);
    if (!rep.pageviews && !(rep.months || []).some((m) => m.pageviews)) {
      body.append(h("div", { class: "card" }, empty(t("visits.none"), t("visits.noneHint"), null, "chart")));
      return;
    }
    const tile = (label, value, hint) => h("div", { class: "stat", title: hint || "" }, h("div", { class: "label" }, label), h("div", { class: "value" }, value));
    const fmt = (n) => Number(n || 0).toLocaleString(currentLang());
    const perVisit = rep.visits ? (rep.pageviews / rep.visits).toLocaleString(currentLang(), { maximumFractionDigits: 1 }) : "—";
    const days = rep.days || [];
    const active = days.filter((d) => d.visitors).length || 1;
    body.append(h("div", { class: "card" },
      h("div", { class: "stats site-stats" },
        rep.unique ? tile(t("visits.unique"), fmt(rep.unique), t("visits.uniqueHint")) : null,
        tile(t("visits.visits"), fmt(rep.visits), t("visits.visitsHint")),
        tile(t("visits.perDay"), fmt(Math.round(rep.visits / active))),
        tile(t("visits.pageviews"), fmt(rep.pageviews)),
        tile(t("visits.perVisit"), perVisit)),
      h("h3", null, t("visits.byDay")),
      bars(days.map((d) => ({ value: d.visitors, label: t("visits.barDay", { date: new Date(d.date + "T12:00").toLocaleDateString(currentLang(), { weekday: "short", day: "numeric", month: "short" }), n: d.visitors, p: d.pageviews }) })),
        days.length ? [days[0].date, days[days.length - 1].date].map((d) => new Date(d + "T12:00").toLocaleDateString(currentLang(), { day: "numeric", month: "short" })) : []),
      h("h3", null, t("visits.byHour")),
      bars((rep.hours || []).map((n, i) => ({ value: n, label: t("visits.barHour", { hour: String(i).padStart(2, "0"), n }) })), ["00", "12", "23"])));

    const share = (rows, name, title, total) => {
      if (!rows || !rows.length) return null;
      const sum = total || rows.reduce((a, r) => a + r.count, 0) || 1;
      const top = rows.slice(0, 12);
      return h("div", { class: "card" }, h("h3", null, title),
        table([{ label: "" }, { label: t("visits.count"), class: "num" }, { label: "%", class: "num" }],
          top.map((r) => h("tr", null,
            cell(name(r.key), null, "primary"),
            cell(fmt(r.count), t("visits.count"), "num mono"),
            cell(h("span", { class: "visits-share" }, shareBar(r.count / sum), Math.round((100 * r.count) / sum) + "%"), "%", "num mono")))).el,
        rows.length > 12 ? h("p", { class: "small muted" }, t("visits.more", { n: rows.length - 12 })) : null);
    };
    const region = (k) => { const [c, r] = k.split("/"); return `${r} (${country(c)})`; };
    const ref = (k) => (k === "(direct)" ? t("visits.direct") : k);
    const other = (map) => (k) => map[k] || (k === "other" ? t("visits.other") : k);
    body.append(h("div", { class: "grid-2" },
      share(rep.countries, country, t("visits.countries"), rep.visits),
      share(rep.regions, region, t("visits.regions"), rep.visits),
      share(rep.devices, (k) => t("visits.device." + k), t("visits.devices")),
      share(rep.browsers, other(BROWSERS), t("visits.browsers")),
      share(rep.os, other(SYSTEMS), t("visits.systems")),
      share(rep.referrers, ref, t("visits.referrers")),
      share(rep.pages, (k) => h("code", { class: "small" }, k), t("visits.pages"), rep.pageviews),
      (rep.hosts || []).length > 1 ? share(rep.hosts, (k) => k, t("visits.hosts")) : null));
    if (!(rep.countries || []).length) body.append(h("p", { class: "small muted" }, t("visits.noCountry")));

    const months = rep.months || [];
    if (months.length) {
      body.append(h("div", { class: "card" }, h("h3", null, t("visits.months")),
        table([{ label: t("visits.month") }, { label: t("visits.unique"), class: "num" }, { label: t("visits.visits"), class: "num" }, { label: t("visits.pageviews"), class: "num" }],
          months.map((m) => {
            const a = h("a", { href: "#" }, monthLabel(m.month));
            a.addEventListener("click", (e) => { e.preventDefault(); period.value = "month:" + m.month; load(); });
            return h("tr", null, cell(a, null, "primary"), cell(fmt(m.unique), t("visits.unique"), "num mono"), cell(fmt(m.visits), t("visits.visits"), "num mono"), cell(fmt(m.pageviews), t("visits.pageviews"), "num mono"));
          }), { responsive: true }).el));
    }
  }

  period.addEventListener("change", load);
  load();
  return { el };
}

// bars is a column per value, its line on hover, the first and last
// labels under it. Attributes only, no style: the CSP drops inline ones.
function bars(items, ticks) {
  const W = 600, H = 120, gap = items.length > 60 ? 1 : 2;
  const max = Math.max(1, ...items.map((i) => i.value || 0));
  const w = W / Math.max(1, items.length);
  const chart = svg("svg", { class: "visits-bars", viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: "none", role: "img", "aria-label": t("visits.chartLabel", { max }) });
  chart.append(svg("line", { class: "base", x1: 0, x2: W, y1: H - 0.5, y2: H - 0.5 }));
  items.forEach((it, i) => {
    const bh = it.value ? Math.max(2, ((it.value || 0) / max) * (H - 4)) : 0;
    // The whole column takes the hover, not only the bar: a day with one
    // visitor is two pixels high.
    const g = svg("g", null, svg("title", null, it.label),
      svg("rect", { class: "hit", x: (i * w).toFixed(2), y: 0, width: w.toFixed(2), height: H }),
      svg("rect", { class: "bar", x: (i * w + gap / 2).toFixed(2), y: (H - bh).toFixed(2), width: Math.max(1, w - gap).toFixed(2), height: bh.toFixed(2) }));
    chart.append(g);
  });
  return h("div", { class: "visits-chart" }, h("span", { class: "peak" }, t("visits.peak", { n: max })), chart,
    h("div", { class: "visits-ticks small muted" }, ...ticks.map((x) => h("span", null, x))));
}

// shareBar is a share as a short bar: an SVG, because a width written as a
// style would be dropped by the CSP, and a <meter> paints itself green,
// amber or red by the browser's own idea of good.
function shareBar(f) {
  const w = Math.max(0, Math.min(1, f)) * 64;
  return svg("svg", { class: "visits-meter", viewBox: "0 0 64 6", width: 64, height: 6, "aria-hidden": "true" },
    svg("rect", { class: "track", x: 0, y: 0, width: 64, height: 6, rx: 3 }),
    svg("rect", { class: "fill", x: 0, y: 0, width: w.toFixed(1), height: 6, rx: 3 }));
}
