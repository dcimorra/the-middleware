// WAF: the global policy, who is blocked, and traffic for the last minute.
// Traffic is the one thing polled (every 2s, only while this screen is
// open): a per-second counter does not belong on the event stream, where
// it would drown everything that matters.
//
// The order is the order of need. Under attack is the switch reached for
// in a hurry, so it is first and on its own: it sat inside the policy
// form, under a Save it does not wait for. Then what is happening (the
// traffic, who is blocked), then the settings, the rare ones folded.

import { icon } from "../lib/icons.js";
import { h, clear, fmtTime, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { onEvent } from "../lib/stream.js";
import { page, load, table, cell } from "../components/page.js";
import { sparkline } from "../components/sparkline.js";
import { open, confirm, field, toggle } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { dirtySave } from "./inbound.js";

// Go writes a duration as 1m0s or 1h0m0s; a person writes 1m and 1h,
// and both are read back the same.
const human = (d) => String(d || "").replace(/(\d)m0s$/, "$1m").replace(/(\d)h0m$/, "$1h");

export function wafScreen() {
  const blockBtn = h("button", { type: "button", class: "btn primary", "data-shortcut": "new" }, icon("ban"), t("waf.blockTitle"));
  const p = page({ title: t("nav.waf"), sub: t("waf.sub"), icon: "shield", actions: blockBtn });
  p.el.classList.add("np-page");
  const attackCard = h("div", { class: "card np-attack" });
  const trafficCard = h("div", { class: "card" });
  const blockedCard = h("div", { class: "card pad0" });
  const banCard = h("div", { class: "card" });
  const policyCard = h("div", { class: "card" });
  p.body.append(attackCard, trafficCard, blockedCard, banCard, policyCard);
  blockBtn.addEventListener("click", () => blockDialog());
  const subs = [];
  let timer = null;

  // ---- traffic ----
  const spark = sparkline({ label: t("waf.perSecond") });
  const byDomain = h("div");
  const byStatus = h("div", { class: "inline-list" });
  const trafficErr = h("span", { class: "small level-error" });
  const nums = h("div", { class: "np-figures" });
  trafficCard.append(h("h2", null, t("waf.traffic")), nums, spark.el,
    h("div", { class: "grid-2 np-traffic-split" }, h("div", null, h("h3", null, t("waf.byDomain")), byDomain), h("div", null, h("h3", null, t("waf.byStatus")), byStatus)), trafficErr);
  const figure = (value, label) => h("div", { class: "np-figure" }, h("strong", null, value), h("span", null, label));

  async function traffic() {
    try {
      const d = await api.get("/waf/traffic");
      // The line is the minute's seconds. It read per_second, which is the
      // minute's average as one number, so the chart had no points and the
      // card showed an empty box; an older build sent the seconds there.
      const ps = Array.isArray(d.seconds) ? d.seconds : Array.isArray(d.per_second) ? d.per_second : [];
      spark.set(ps);
      clear(nums).append(
        figure(String(d.requests ?? ps.reduce((a, b) => a + b, 0)), t("waf.lastMinute")),
        figure(Number(typeof d.per_second === "number" ? d.per_second : 0).toFixed(1), t("waf.avgPerSecond")),
        figure(String(ps.length ? Math.max(...ps) : 0), t("waf.busiest")));
      // Each domain is {requests, per_second, p95_ms…}; a bare number is
      // an older build's answer.
      const reqs = (v) => (typeof v === "number" ? v : (v && v.requests) || 0);
      const dom = Object.entries(d.by_domain || {}).sort((a, b) => reqs(b[1]) - reqs(a[1])).slice(0, 12);
      clear(byDomain);
      if (dom.length === 0) byDomain.append(h("p", { class: "np-none" }, t("waf.noTraffic")));
      else byDomain.append(table([{ label: t("waf.domain") }, { label: t("waf.requests"), class: "num" }], dom.map(([k, v]) => h("tr", null, cell(h("a", { href: `#/sites/${encodeURIComponent(k)}/activity` }, k)), cell(String(reqs(v)), null, "num mono"))), {}).el);
      clear(byStatus);
      for (const [k, v] of Object.entries(d.by_status || {}).sort()) {
        const cls = k.startsWith("5") ? "bad" : k.startsWith("4") ? "warn" : k.startsWith("2") ? "ok" : "";
        byStatus.append(h("span", { class: "badge " + cls }, `${k}: ${v}`));
      }
      if (!byStatus.firstChild) byStatus.append(h("span", { class: "muted small" }, "—"));
      trafficErr.textContent = "";
    } catch (e) {
      trafficErr.textContent = e.message;
    }
  }

  // ---- under attack ----
  function attack(policy) {
    const on = Boolean((policy.attack && policy.attack.on) || policy.under_attack);
    const sw = toggle(t("waf.underAttack"), { checked: on });
    const paint = () => attackCard.classList.toggle("on", sw.input.checked);
    sw.input.addEventListener("change", async () => {
      sw.input.disabled = true;
      try {
        await api.post("/waf/under-attack", { enabled: sw.input.checked });
        toast(sw.input.checked ? t("waf.underAttackOn") : t("waf.underAttackOff"), "ok");
      } catch (e) { sw.input.checked = !sw.input.checked; toastError(e); }
      finally { sw.input.disabled = false; paint(); }
    });
    paint();
    return h("div", { class: "np-link-head" },
      h("span", { class: "np-link-icon" }, icon("shield")),
      h("div", { class: "grow" }, h("h2", null, t("waf.attackTitle")), h("p", null, t("waf.underAttackHint"))),
      h("div", { class: "np-attack-switch" }, sw.el));
  }

  // ---- policy ----
  function policyForm(policy) {
    const rate = policy.rate || {};
    const requests = h("input", { type: "number", min: "0", inputmode: "numeric", value: rate.requests ?? "" });
    const window_ = h("input", { type: "text", class: "mono", placeholder: "1m", value: human(rate.window) });
    const burst = h("input", { type: "number", min: "0", inputmode: "numeric", value: rate.burst ?? "" });
    const maxBody = h("input", { type: "text", class: "mono", placeholder: "20m", value: policy.max_body || "" });
    const deny = h("input", { type: "text", class: "mono", placeholder: "CN, RU", value: (policy.deny_countries || []).join(", ") });
    const agents = h("textarea", { class: "mono", rows: "3", value: (policy.deny_agents || []).join("\n") });
    const trips = h("input", { type: "number", min: "0", inputmode: "numeric", value: policy.trips ?? "" });
    const save = h("button", { type: "submit", class: "btn" }, t("common.save"));
    const err = h("div", { class: "error-box", hidden: true });
    // The body limit, countries and agents are set once if ever: folded,
    // and open when one of them already says something.
    const more = h("details", { class: "np-fold", open: Boolean(policy.max_body && policy.max_body !== "20m") || Boolean((policy.deny_countries || []).length || (policy.deny_agents || []).length) },
      h("summary", null, t("waf.moreRules")),
      h("div", { class: "np-fold-body np-fold-form" },
        h("div", { class: "form-row" }, field(t("access.maxBody"), maxBody), field(t("access.denyCountries"), deny, t("access.denyHint"))),
        field(t("waf.denyAgents"), agents, t("waf.denyAgentsHint"))));
    const form = h("form", { novalidate: true },
      h("div", { class: "form-row np-row-4" },
        field(t("access.rateRequests"), requests), field(t("access.rateWindow"), window_), field(t("access.rateBurst"), burst), field(t("waf.trips"), trips)),
      // The hint of "block after" is a paragraph: under its own 160px
      // field it was nine lines tall and pushed the form apart.
      h("p", { class: "hint np-row-hint" }, t("waf.tripsHint")),
      more,
      err,
      h("div", { class: "form-actions" }, save),
    );
    const clean = dirtySave(form, save);
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      const out = { ...policy };
      const r = {};
      if (requests.value !== "") r.requests = Number(requests.value);
      if (window_.value) r.window = window_.value.trim();
      if (burst.value !== "") r.burst = Number(burst.value);
      if (Object.keys(r).length) out.rate = r; else delete out.rate;
      if (maxBody.value) out.max_body = maxBody.value.trim(); else delete out.max_body;
      const dc = deny.value.split(",").map((x) => x.trim().toUpperCase()).filter(Boolean);
      if (dc.length) out.deny_countries = dc; else delete out.deny_countries;
      const ua = agents.value.split("\n").map((x) => x.trim()).filter(Boolean);
      if (ua.length) out.deny_agents = ua; else delete out.deny_agents;
      if (trips.value !== "") out.trips = Number(trips.value); else delete out.trips;
      save.classList.add("busy");
      err.hidden = true;
      try { await api.put("/waf", out); clean(); toast(t("waf.saved"), "ok"); }
      catch (ex) { err.textContent = ex.message; err.hidden = false; }
      finally { save.classList.remove("busy"); }
    });
    return form;
  }

  // ---- blocked ----
  function blockedTable(blocked, recent) {
    const rows = (blocked || []).map((b) => h("tr", null,
      cell(h("code", null, b.ip), null, "primary"),
      cell(b.reason || "", t("waf.reason")),
      cell(b.until ? h("span", { title: fmtTime(b.until) }, fmtAgo(b.until, t)) : t("waf.forever"), t("waf.until")),
      cell(unblockBtn(b.ip), null, "right"),
    ));
    const recentRows = (recent || []).slice(0, 20).map((r) => h("tr", null,
      cell(h("code", null, r.ip), null, "primary"),
      cell(String(r.hits ?? ""), t("waf.hits"), "num mono"),
      cell(r.status ? h("span", { class: "badge " + (r.status === "blocked" ? "bad" : r.status === "limited" ? "warn" : "") }, r.status) : "", t("waf.status")),
      cell(r.status === "blocked" ? "" : blockQuick(r.ip), null, "right"),
    ));
    const head = h("div", { class: "log-bar card-pad np-card-head" }, h("h2", { class: "grow" }, t("waf.blocked"), rows.length ? h("span", { class: "np-quiet" }, String(rows.length)) : null));
    return [
      head,
      rows.length ? table([{ label: t("waf.address") }, { label: t("waf.reason") }, { label: t("waf.until") }, { label: "" }], rows, { responsive: true }).el
        : h("p", { class: "np-none card-pad" }, icon("check", { size: 16 }), h("span", null, h("strong", null, t("waf.noneBlocked")), " ", t("waf.noneBlockedHint"))),
      recentRows.length ? [h("div", { class: "log-bar card-pad np-card-head np-sub-card-head" }, h("h2", null, t("waf.recent"))), table([{ label: t("waf.address") }, { label: t("waf.hits"), class: "num" }, { label: t("waf.status") }, { label: "" }], recentRows, { responsive: true }).el] : null,
    ];
  }
  function unblockBtn(ip) {
    const b = h("button", { type: "button", class: "btn sm" }, t("waf.unblock"));
    b.addEventListener("click", async () => {
      if (!(await confirm({ title: t("waf.unblockQ", { ip }), confirmLabel: t("waf.unblock") }))) return;
      try { await api.del(`/waf/block/${enc(ip)}`); toast(t("waf.unblocked", { ip }), "ok"); refresh(); } catch (e) { toastError(e); }
    });
    return b;
  }
  function blockQuick(ip) {
    const b = h("button", { type: "button", class: "btn sm ghost" }, t("waf.block"));
    b.addEventListener("click", () => blockDialog(ip));
    return b;
  }
  async function blockDialog(presetIp) {
    const ip = h("input", { type: "text", class: "mono", value: typeof presetIp === "string" ? presetIp : "", placeholder: "203.0.113.7", autocomplete: "off" });
    const duration = h("input", { type: "text", class: "mono", value: "1h" });
    const reason = h("input", { type: "text" });
    const done = await open({
      title: t("waf.blockTitle"),
      body: [field(t("waf.address"), ip), field(t("waf.duration"), duration, t("waf.durationHint")), field(t("waf.reason"), reason)],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("waf.block"), submit: true, primary: true, danger: true }],
      onSubmit: async (_f, showError) => {
        const v = ip.value.trim();
        if (!v) { showError(t("waf.addressRequired")); return false; }
        await api.post("/waf/block", { ip: v, duration: duration.value.trim(), reason: reason.value.trim() });
      },
    });
    if (done) { toast(t("waf.blockedNow", { ip: ip.value.trim() }), "ok"); refresh(); }
  }

  // ---- bans (fail2ban) ----
  // The settings of the bans and who is being watched: addresses with
  // strikes inside the window, not banned yet.
  function banForm(global, ban, watched) {
    const on = toggle(t("ban.on"), { checked: !ban.off });
    const num = (v, min, max) => h("input", { type: "number", min: String(min), max: String(max), step: "1", inputmode: "numeric", value: String(v) });
    const retry = num(ban.max_retry, 1, 1000), find = num(ban.find_minutes, 1, 10080), first = num(ban.ban_minutes, 1, 527040);
    const factor = num(ban.factor, 1, 100), ceiling = num(ban.max_ban_days, 1, 366);
    const save = h("button", { type: "submit", class: "btn" }, t("common.save"));
    const err = h("div", { class: "error-box", hidden: true });
    const numbers = h("div", { class: "np-fold-form" },
      h("div", { class: "form-row np-row-5" },
        field(t("ban.retry"), retry), field(t("ban.find"), find), field(t("ban.first"), first),
        field(t("ban.factor"), factor, t("ban.factorHint")), field(t("ban.ceiling"), ceiling)),
      err, h("div", { class: "form-actions" }, save));
    const form = h("form", { novalidate: true },
      h("div", { class: "field" }, on.el, h("div", { class: "hint" }, t("ban.onHint"))),
      numbers,
      h("details", { class: "np-fold" }, h("summary", null, t("ban.whatTitle")), h("div", { class: "np-fold-body" }, h("p", null, t("ban.what")))));
    const clean = dirtySave(form, save);
    numbers.hidden = ban.off;
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      const ints = [retry, find, first, factor, ceiling].map((i) => Number(i.value));
      if (ints.some((n) => !Number.isInteger(n) || n < 1)) { err.textContent = t("ban.bad"); err.hidden = false; return; }
      const [max_retry, find_minutes, ban_minutes, f, max_ban_days] = ints;
      save.classList.add("busy");
      err.hidden = true;
      try {
        await api.put("/waf", { ...global, ban: { off: !on.input.checked, max_retry, find_minutes, ban_minutes, factor: f, max_ban_days } });
        clean();
        toast(t("ban.saved"), "ok");
      } catch (ex) { err.textContent = ex.message; err.hidden = false; }
      finally { save.classList.remove("busy"); }
    });
    // Off, the numbers mean nothing: they hide with it. The switch saves
    // at once, after the change has reached the form (and marked it
    // unsaved), so the save then marks it saved and not the other way.
    on.input.addEventListener("change", () => { numbers.hidden = !on.input.checked; setTimeout(() => form.requestSubmit()); });
    const rows = (watched || []).map((w) => {
      const last = (w.offenses || [])[w.offenses.length - 1] || {};
      return h("tr", null,
        cell(h("span", { class: "mono" }, w.ip), null, "primary"),
        cell(`${w.score} / ${ban.max_retry}`, t("ban.score"), "num"),
        cell(h("span", { class: "small" }, last.reason || "", " ", h("span", { class: "mono muted" }, (last.host || "") + (last.path || ""))), t("ban.last")),
        cell(h("span", { class: "small muted", title: fmtTime(w.first) }, fmtAgo(w.first, t)), t("ban.since")),
        cell(blockQuick(w.ip), null, "right"));
    });
    return [h("h2", null, t("ban.title")), h("p", { class: "np-lead" }, t("ban.sub")), form,
      h("h3", { class: "np-h3" }, t("ban.watched")),
      rows.length ? table([{ label: t("waf.address") }, { label: t("ban.score"), class: "num" }, { label: t("ban.last") }, { label: t("ban.since") }, { label: "" }], rows, { responsive: true }).el
        : h("p", { class: "np-none" }, icon("check", { size: 16 }), h("span", null, t("ban.none")))];
  }

  async function refresh() {
    const d = await api.get("/waf");
    // A form being edited is not redrawn under the operator's fingers by
    // an event from elsewhere.
    const editing = (card) => card.querySelector("form button[type=submit]:not(:disabled)");
    clear(attackCard).append(attack(d.global || {}));
    if (!editing(policyCard)) clear(policyCard).append(h("h2", null, t("waf.policy")), h("p", { class: "np-lead" }, t("waf.policyHint")), policyForm(d.global || {}));
    if (!editing(banCard)) clear(banCard).append(...banForm(d.global || {}, d.ban || {}, d.watched).filter(Boolean));
    clear(blockedCard).append(...[blockedTable(d.blocked, d.recent)].flat(Infinity).filter(Boolean));
    return null;
  }

  load(policyCard, refresh);
  traffic();
  timer = setInterval(() => { if (!document.hidden) traffic(); }, 2000);
  subs.push(onEvent("waf.", () => refresh().catch(() => {})));
  return { el: p.el, destroy() { clearInterval(timer); subs.forEach((u) => u()); } };
}
