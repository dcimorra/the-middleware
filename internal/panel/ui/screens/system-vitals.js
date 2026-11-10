// The machine live: processor (with the disk wait apart), memory, swap
// and disk over the last hour, and the stretches it spent under strain
// with who caused them. Read every five seconds while the card is shown.

import { h, clear, svg, fmtBytes } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api } from "../lib/api.js";
import { notice } from "../components/page.js";
import { cardHead } from "./settings-kit.js";

// A line over the hour, 0 to 100, with the threshold drawn.
function chart(values, line, label) {
  const W = 600, H = 70, n = values.length;
  const s = svg("svg", { class: "vchart", viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: "none", role: "img", "aria-label": label });
  if (line) s.append(svg("line", { class: "limit", x1: 0, x2: W, y1: H - (line / 100) * H, y2: H - (line / 100) * H }));
  if (n > 1) {
    let d = "";
    values.forEach((v, i) => { d += (i ? "L" : "M") + ((i / (n - 1)) * W).toFixed(1) + " " + (H - (Math.min(100, Math.max(0, v)) / 100) * (H - 2) - 1).toFixed(1) + " "; });
    s.append(svg("path", { class: "area", d: `${d} L${W} ${H} L0 ${H} Z` }), svg("path", { class: "line", d }));
  }
  return s;
}

export function vitalsCard() {
  const el = h("div", { class: "card sys-card" });
  const pct = (v) => `${Number(v || 0).toFixed(0)}%`;
  async function load() {
    let v;
    try { v = await api.get("/system/vitals"); } catch (e) { clear(el).append(cardHead(t("vitals.title"), null), notice("bad", e.message)); return; }
    const hist = v.history || [];
    const n = v.now || {};
    const row = (label, value, values, line, extra) => h("div", { class: "vrow" },
      h("div", { class: "vhead" }, h("span", null, label), h("strong", { class: "mono" }, value), extra ? h("span", { class: "small muted" }, extra) : null),
      chart(values, line, label));
    // Three charts side by side where there is room: stacked, the hour
    // of the machine was 400px before anything else on the screen.
    clear(el).append(cardHead(t("vitals.title"), t("vitals.hint")), h("div", { class: "sys-vitals" },
      row(t("vitals.cpu"), pct(n.cpu), hist.map((s) => s.cpu), v.lines && v.lines.cpu,
        t("vitals.cpuExtra", { cores: n.cores || 0, iowait: pct(n.iowait), load: Number(n.load1 || 0).toFixed(2) })),
      row(t("vitals.memory"), pct(n.mem_pct), hist.map((s) => s.mem_pct), v.lines && v.lines.memory,
        `${fmtBytes(n.mem_used)} / ${fmtBytes(n.mem_total)}` + (n.swap_total ? " · " + t("vitals.swap", { used: fmtBytes(n.swap_used), total: fmtBytes(n.swap_total) }) : "")),
      row(t("vitals.disk"), pct(n.disk_pct), hist.map((s) => s.disk_pct), 0, `${fmtBytes(n.disk_used)} / ${fmtBytes(n.disk_total)}`)));
    const eps = v.episodes || [];
    el.append(h("h3", { class: "sys-episodes-h" }, t("vitals.episodes")));
    if (!eps.length) { el.append(h("p", { class: "hint" }, t("vitals.noEpisodes"))); return; }
    for (const e of eps.slice(0, 10)) {
      const when = new Date(e.from).toLocaleString() + (e.to && !e.to.startsWith("0001") ? " → " + new Date(e.to).toLocaleTimeString() : " · " + t("vitals.ongoing"));
      el.append(h("details", { class: "episode" },
        h("summary", null, h("strong", null, t("vitals.reason." + e.reason)), " ", h("span", { class: "small muted" }, when), " ",
          h("span", { class: "small" }, t("vitals.peak", { cpu: pct(e.peak_cpu), mem: pct(e.peak_mem), iowait: pct(e.iowait_max) }))),
        (e.blamed || []).length ? h("ul", { class: "small" }, ...e.blamed.map((b) => h("li", null,
          b.site ? h("a", { href: `#/sites/${encodeURIComponent(b.site)}` }, b.site) : h("span", { class: "muted" }, t("vitals.noSite")), " · ",
          h("code", null, b.cmd), " ", h("span", { class: "muted" }, t("vitals.blame", { times: b.times, cpu: pct(b.cpu) })))))
          : h("p", { class: "small muted" }, t("vitals.noBlame"))));
    }
  }
  load();
  const timer = setInterval(() => { if (!document.hidden) load(); }, 5000);
  return { el, destroy() { clearInterval(timer); } };
}
