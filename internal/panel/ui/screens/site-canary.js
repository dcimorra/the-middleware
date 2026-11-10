// Canary deploys on the site page: the switch with its share and time
// (manifest run.canary) and, while a new generation serves part of the
// visitors, both sides' numbers and the two ways to end it now. The
// judgement is the engine's (internal/engine, canary.go).

import { h, clear, fmtTime } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { store } from "../lib/store.js";
import { onEvent } from "../lib/stream.js";
import { toggle } from "../components/dialog.js";
import { notice } from "../components/page.js";
import { toast, toastError } from "../components/toast.js";

export function canaryCard(domain, m) {
  const run = m.run || {};
  const cfg = run.canary || null;
  const el = h("section", { class: "card site-section" });
  const live = h("div");
  const sw = toggle(t("canary.on"), { checked: Boolean(cfg) });
  const percent = h("input", { type: "number", min: 1, max: 99, value: (cfg && cfg.percent) || 10, class: "canary-num", "aria-label": t("canary.percent") });
  const minutes = h("input", { type: "number", min: 1, max: 1440, value: cfg && cfg.for ? Math.max(1, Math.round(parseDur(cfg.for) / 60)) : 10, class: "canary-num", "aria-label": t("canary.minutes") });
  const settings = h("p", { class: "small" }, t("canary.share"), " ", percent, " % · ", t("canary.during"), " ", minutes, " min");
  settings.hidden = !cfg;

  async function save() {
    // Read again: the manifest may have changed since the page was drawn.
    const now = (store.site(domain).manifest || {}).run || {};
    const next = sw.input.checked
      ? { ...now, canary: { ...(now.canary || {}), percent: clamp(+percent.value, 1, 99), for: clamp(+minutes.value, 1, 1440) + "m" } }
      : (() => { const r = { ...now }; delete r.canary; return r; })();
    try {
      await api.patch(`/sites/${enc(domain)}/manifest`, { run: next });
      toast(sw.input.checked ? t("canary.saved") : t("canary.off"), "ok");
    } catch (e) { toastError(e); }
  }
  sw.input.addEventListener("change", () => { settings.hidden = !sw.input.checked; save(); });
  percent.addEventListener("change", save);
  minutes.addEventListener("change", save);

  const pct = (e, n) => (n ? ((100 * e) / n).toFixed(1) + "%" : "—");
  async function load() {
    let st;
    try { st = await api.get(`/sites/${enc(domain)}/canary`); } catch { return; }
    clear(live);
    const c = st.running;
    if (c) {
      const decide = (what, label, primary) => {
        const b = h("button", { type: "button", class: "btn" + (primary ? " primary" : "") }, label);
        b.addEventListener("click", async () => {
          b.disabled = true;
          try { await api.post(`/sites/${enc(domain)}/canary/${what}`, {}); setTimeout(load, 1500); } catch (e) { toastError(e); b.disabled = false; }
        });
        return b;
      };
      live.append(notice("info", t("canary.running", { percent: c.percent, until: fmtTime(c.until) })),
        h("p", { class: "small mono" }, t("canary.sides", { n: c.requests, e: pct(c.errors, c.requests), sn: c.stable_requests, se: pct(c.stable_errors, c.stable_requests) })),
        h("div", { class: "row-actions" }, decide("promote", t("canary.promote"), true), decide("rollback", t("canary.rollback"), false)));
    } else if (st.failed) {
      live.append(notice("warn", t("canary.failed", { when: fmtTime(st.failed.at), reason: st.failed.reason })));
    }
  }

  el.append(h("div", { class: "log-bar" }, h("h2", { class: "grow" }, t("canary.title")), sw.el),
    h("p", { class: "small muted" }, t("canary.hint")), settings, live);
  load();
  const mine = (ev) => { if (ev.subject === domain) load(); };
  const offs = [onEvent("canary.", mine), onEvent("handover.", mine)];
  // While one runs its numbers move; the page is left, the timer goes.
  const timer = setInterval(() => {
    if (!el.isConnected) { clearInterval(timer); offs.forEach((f) => f()); return; }
    if (live.firstChild && !document.hidden) load();
  }, 5000);
  return el;
}

function clamp(n, lo, hi) { return Math.min(hi, Math.max(lo, Math.round(n) || lo)); }

// parseDur reads the manifest's "10m", "90s", "1h30m" into seconds.
function parseDur(s) {
  let total = 0;
  for (const [, n, u] of String(s).matchAll(/(\d+(?:\.\d+)?)(h|m|s)/g)) total += +n * ({ h: 3600, m: 60, s: 1 }[u]);
  return total || 600;
}
