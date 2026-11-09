// The network's autonomous agent: its switch and the kill switch, where
// it stands (the session's context, the subscription's windows, its next
// wake), the sites it made, the guidelines it follows, its limits, its
// notebook, its journal and its sessions. It acts with nobody watching,
// so everything it did is readable here, and the brakes are here too.

import { icon } from "../lib/icons.js";
import { h, clear, fmtTime, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { onEvent } from "../lib/stream.js";
import { page, load, notice } from "../components/page.js";
import { confirm, field } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { cardHead, saveBar, fold } from "./settings-kit.js";
import { chatTab } from "./site-chat.js";
import { tabs } from "../components/tabs.js";

export function autopilotScreen({ tab } = {}) {
  const p = page({ title: t("nav.autopilot"), sub: t("autopilot.sub"), icon: "sparkles" });
  const active = tab === "sessions" ? "sessions" : "agent";
  p.body.append(tabs([
    { key: "agent", href: "#/autopilot", label: t("autopilot.tab.agent"), icon: "power" },
    { key: "sessions", href: "#/autopilot/sessions", label: t("autopilot.sessions"), icon: "chat" },
  ], active));
  // Its sessions are a tab of their own. The chat makes the page the
  // height of the screen (body.chat-fit), and below the cards that turned
  // every card into a short box scrolling on its own, with the screen's
  // leftover height empty under them.
  if (active === "sessions") return sessionsTab(p);
  const stateCard = h("div", { class: "card set-card" });
  const proposalsCard = h("div", { class: "card set-card" });
  const sitesCard = h("div", { class: "card set-card" });
  const rulesCard = h("div", { class: "card set-card" });
  const limitsCard = h("div", { class: "card set-card" });
  const bookCard = h("div", { class: "card set-card" });
  const journalCard = h("div", { class: "card set-card" });
  p.body.append(stateCard, proposalsCard, sitesCard, rulesCard, limitsCard, bookCard, journalCard);

  const act = (path, body, msg) => async (e) => {
    // currentTarget is gone once the event is over, and a confirm dialog
    // awaited before this runs outlives it: "Stop now" threw here.
    const btn = e.currentTarget || (e.target && e.target.closest && e.target.closest("button")) || document.createElement("button");
    btn.classList.add("busy");
    try { await api.post("/autopilot/" + path, body); if (msg) toast(msg, "ok"); refresh(); } catch (err) { toastError(err); } finally { btn.classList.remove("busy"); }
  };

  function meter(pct) {
    const fill = h("div", { class: "fill" + (pct >= 80 ? " bad" : pct >= 60 ? " warn" : "") });
    fill.style.width = Math.max(0, Math.min(100, pct)) + "%";
    return h("div", { class: "meter" }, fill);
  }

  function renderState(st) {
    const s = st.state || {};
    const on = st.enabled;
    const toggleBtn = h("button", { type: "button", class: "btn" + (on ? "" : " primary") }, icon(on ? "pause" : "play"), t(on ? "autopilot.off" : "autopilot.on"));
    toggleBtn.addEventListener("click", act(on ? "off" : "on", null, t(on ? "autopilot.offDone" : "autopilot.onDone")));
    const stop = h("button", { type: "button", class: "btn danger" }, icon("stop"), t("autopilot.stop"));
    stop.addEventListener("click", async (e) => {
      if (!(await confirm({ title: t("autopilot.stopQ"), body: t("autopilot.stopBody"), confirmLabel: t("autopilot.stop"), danger: true }))) return;
      act("stop", null, t("autopilot.stopped"))(e);
    });
    const wake = h("button", { type: "button", class: "btn ghost" }, icon("refresh"), t("autopilot.wake"));
    wake.addEventListener("click", act("wake", null, t("autopilot.wakeDone")));
    const lines = [];
    let line = on ? t("autopilot.isOn", { node: st.node || "" }) : t("autopilot.isOff");
    if (on && st.running) line += " · " + t("autopilot.working", { since: fmtAgo(s.turn_started, t) });
    else if (on && s.next_wake) line += " · " + t("autopilot.nextWake", { at: fmtTime(s.next_wake) }) + (s.next_reason ? ` (${s.next_reason})` : "");
    lines.push(h("p", null, h("strong", null, line)));
    if (s.skipped && on) lines.push(notice("warn", t("autopilot.heldBack", { why: s.skipped })));
    if (s.last_result) lines.push(h("p", { class: "small" }, t("autopilot.lastTurn"), " ", s.last_result));
    // The two measures it works by: the session's context and the
    // subscription's windows, the only brake on what it uses.
    const meters = [];
    if (st.context_percent >= 0) {
      meters.push(h("div", { class: "usage-row" }, h("div", { class: "row" }, h("span", { class: "grow" }, t("autopilot.context")), h("span", { class: "muted small" }, t("autopilot.contextOf", { pct: st.context_percent, used: (s.context || 0).toLocaleString(), window: (s.window || 0).toLocaleString(), at: st.limits.context_percent }))), meter(st.context_percent)));
    }
    for (const w of st.quota || []) {
      if (w.used < 0) continue;
      meters.push(h("div", { class: "usage-row" }, h("div", { class: "row" }, h("span", { class: "grow" }, t("autopilot.window", { name: w.label || w.name })), h("span", { class: "muted small" }, t("autopilot.windowOf", { pct: Math.round(w.used), stop: st.limits.quota_percent }) + (w.resets_at ? " · " + t("autopilot.resets", { at: fmtTime(w.resets_at) }) : ""))), meter(w.used)));
    }
    if (!meters.length) meters.push(h("p", { class: "hint" }, t("autopilot.noMeters")));
    const delegated = (s.delegated || []).map((d) => h("li", null, h("a", { href: `#/sites/${enc(d.domain)}/chat` }, d.domain), " ", h("span", { class: "muted" }, t("autopilot.since", { at: fmtAgo(d.at, t) }))));
    // The other nodes and the move: one agent for the whole network.
    const net = st.network || [];
    let move = null;
    if (net.length) {
      const sel = h("select", null, ...net.filter((n) => n.ok).map((n) => h("option", { value: n.id }, n.name)));
      const go = h("button", { type: "button", class: "btn sm" }, t("autopilot.move"));
      go.addEventListener("click", async (e) => {
        const name = sel.selectedOptions[0] ? sel.selectedOptions[0].textContent : "";
        if (!name || !(await confirm({ title: t("autopilot.moveQ", { node: name }), body: t("autopilot.moveBody"), confirmLabel: t("autopilot.move") }))) return;
        act("move", { node: sel.value }, t("autopilot.moved", { node: name }))(e);
      });
      move = h("div", { class: "set-recent" }, h("h3", null, t("autopilot.network")),
        h("ul", { class: "set-recent-list" }, ...net.map((n) => h("li", null, h("strong", null, n.name),
          h("span", { class: n.ok ? "" : "level-error" }, n.ok ? t("autopilot.nodeSites", { n: (n.sites || []).length, trouble: n.trouble || 0 }) : t("autopilot.nodeDown"))))),
        net.some((n) => n.ok) ? h("div", { class: "form-row" }, sel, go) : null);
    }
    clear(stateCard).append(...[
      cardHead(t("autopilot.title"), t("autopilot.hint"), toggleBtn, stop, wake),
      ...lines, ...meters,
      delegated.length ? h("div", { class: "set-recent" }, h("h3", null, t("autopilot.atWork")), h("ul", { class: "set-recent-list" }, ...delegated)) : null,
      move].filter(Boolean));
  }

  // What it found and may not change itself: each a card with the two
  // decisions the operator has, so a finding never waits unread in its
  // notebook again.
  function renderProposals(st) {
    const all = (st.state && st.state.proposals) || [];
    const open = all.filter((x) => x.status === "open" || x.status === "failed");
    const rest = all.filter((x) => !(x.status === "open" || x.status === "failed")).slice(0, 10);
    const convLink = (x) => {
      const [domain, id] = (x.conversation || "").split("#");
      const href = domain === "middleware" ? "#/house" : `#/sites/${enc(domain)}/chat?c=${enc(id || "")}`;
      return h("a", { href }, t("autopilot.proposal.openChat"));
    };
    const item = (x) => {
      const where = x.domain ? h("a", { href: `#/sites/${enc(x.domain)}` }, x.domain) : h("span", null, t("autopilot.proposal.house"));
      const cls = { open: "", sent: "level-warn", fixed: "level-ok", failed: "level-error", dismissed: "muted" }[x.status] || "";
      const li = h("li", { class: "proposal" },
        h("div", { class: "proposal-head" },
          h("span", { class: "badge " + cls }, t("autopilot.proposal.status." + x.status)),
          where,
          h("span", { class: "muted", title: fmtTime(x.at) }, fmtAgo(x.at, t))),
        h("strong", { class: "proposal-title" }, x.title),
        h("details", { open: x.status === "open" }, h("summary", { class: "small muted" }, t("autopilot.proposal.read")),
          h("pre", { class: "mono small pre-wrap" }, x.text)));
      if (x.outcome) li.append(h("p", { class: "small" }, x.status === "dismissed" ? t("autopilot.proposal.why") : t("autopilot.proposal.outcome"), " ", x.outcome));
      if (x.conversation && x.status !== "dismissed") li.append(h("p", { class: "small" }, convLink(x)));
      if (x.status === "open" || x.status === "failed") {
        const send = h("button", { type: "button", class: "btn sm primary" }, icon("play"), t(x.status === "failed" ? "autopilot.proposal.retry" : "autopilot.proposal.send"));
        send.addEventListener("click", async (e) => {
          const note = h("textarea", { rows: 3, placeholder: t("autopilot.proposal.notePlaceholder") });
          if (!(await confirm({ title: t("autopilot.proposal.sendQ"), body: t("autopilot.proposal.sendBody", { where: x.domain || t("autopilot.proposal.house") }), confirmLabel: t("autopilot.proposal.send"), extra: field(t("autopilot.proposal.note"), note) }))) return;
          act(`proposals/${enc(x.id)}/send`, { note: note.value }, t("autopilot.proposal.sent"))(e);
        });
        const drop = h("button", { type: "button", class: "btn sm ghost" }, t("autopilot.proposal.dismiss"));
        drop.addEventListener("click", async (e) => {
          const note = h("textarea", { rows: 2, placeholder: t("autopilot.proposal.whyPlaceholder") });
          if (!(await confirm({ title: t("autopilot.proposal.dismissQ"), body: t("autopilot.proposal.dismissBody"), confirmLabel: t("autopilot.proposal.dismiss"), extra: field(t("autopilot.proposal.why"), note) }))) return;
          act(`proposals/${enc(x.id)}/dismiss`, { note: note.value }, null)(e);
        });
        li.append(h("div", { class: "form-row" }, send, drop));
      }
      return li;
    };
    const parts = [cardHead(t("autopilot.proposals"), t("autopilot.proposalsHint"))];
    parts.push(open.length ? h("ul", { class: "set-recent-list proposals" }, ...open.map(item)) : h("p", { class: "hint" }, t("autopilot.proposal.none")));
    if (rest.length) parts.push(fold(t("autopilot.proposal.decided", { n: rest.length }), null, [h("ul", { class: "set-recent-list proposals" }, ...rest.map(item))]));
    clear(proposalsCard).append(...parts);
  }

  function renderSites(st) {
    const made = (st.state && st.state.created) || [];
    const rows = made.slice().reverse().map((c) => {
      const state = c.published ? t("autopilot.site.published") : c.waiting ? t("autopilot.site.waiting") : t("autopilot.site.building");
      const li = h("li", null, h("a", { href: `#/sites/${enc(c.domain)}` }, c.domain), h("span", { class: c.waiting ? "level-warn" : "" }, state), h("span", { class: "muted", title: fmtTime(c.at) }, fmtAgo(c.at, t)));
      if (c.waiting) {
        const pub = h("button", { type: "button", class: "btn sm primary" }, t("autopilot.publish"));
        pub.addEventListener("click", act(`sites/${enc(c.domain)}/publish`, null, t("autopilot.publishedToast", { name: c.domain })));
        const no = h("button", { type: "button", class: "btn sm ghost" }, t("autopilot.decline"));
        no.addEventListener("click", act(`sites/${enc(c.domain)}/decline`, null, null));
        li.append(pub, no);
      }
      return li;
    });
    clear(sitesCard).append(cardHead(t("autopilot.sites"), t("autopilot.sitesHint", { left: st.sites_left, per: st.limits.sites_per_week })),
      rows.length ? h("ul", { class: "set-recent-list" }, ...rows) : h("p", { class: "hint" }, t("autopilot.noSites")));
  }

  function renderRules(st) {
    const text = h("textarea", { rows: 16, class: "mono", spellcheck: "true" });
    text.value = st.guidelines || "";
    const save = h("button", { type: "button", class: "btn primary" }, t("common.save"));
    const bar = saveBar(save);
    save.addEventListener("click", async () => {
      save.classList.add("busy");
      try { await api.put("/autopilot/guidelines", { text: text.value }); bar.saved(); } catch (e) { toastError(e); } finally { save.classList.remove("busy"); }
    });
    const reset = h("button", { type: "button", class: "btn ghost" }, t("autopilot.defaults"));
    reset.addEventListener("click", async () => {
      if (!(await confirm({ title: t("autopilot.defaultsQ"), body: t("autopilot.defaultsBody"), confirmLabel: t("autopilot.defaults") }))) return;
      try { await api.put("/autopilot/guidelines", { text: "" }); refresh(); } catch (e) { toastError(e); }
    });
    bar.el.append(reset);
    const form = h("div", null, text, bar.el);
    bar.watch(form);
    clear(rulesCard).append(cardHead(t("autopilot.rules"), t(st.custom_guidelines ? "autopilot.rulesHint" : "autopilot.rulesDefault")), form);
  }

  async function renderLimits(st) {
    const l = st.limits || {};
    const agents = await api.get("/agents").catch(() => ({ providers: [] }));
    const providers = (agents.providers || []).filter((x) => x.available || x.provider === l.provider);
    const prov = h("select", null, ...providers.map((x) => h("option", { value: x.provider }, x.provider)));
    prov.value = l.provider || "claude";
    const model = h("select");
    const fillModels = () => {
      const x = providers.find((y) => y.provider === prov.value);
      clear(model).append(h("option", { value: "" }, t("chat.defaultModel")), ...((x && x.models) || []).map((m) => h("option", { value: m }, (x.labels && x.labels[m]) ? `${m} (${x.labels[m]})` : m)));
      model.value = (x && (x.models || []).includes(l.model)) ? l.model : "";
    };
    prov.addEventListener("change", fillModels);
    fillModels();
    const num = (v, min, max) => h("input", { type: "number", min: String(min), max: String(max), value: String(v) });
    const perWeek = num(l.sites_per_week, 0, 50);
    const zones = h("input", { type: "text", value: (l.zones || []).join(", "), placeholder: t("autopilot.zonesAny") });
    const publish = h("select", null, h("option", { value: "auto" }, t("autopilot.publishAuto")), h("option", { value: "ask" }, t("autopilot.publishAsk")));
    publish.value = l.publish || "auto";
    const minM = num(l.min_minutes, 1, 10080);
    const maxM = num(l.max_minutes, 1, 10080);
    const quota = num(l.quota_percent, 1, 100);
    const ctx = num(l.context_percent, 10, 95);
    const save = h("button", { type: "button", class: "btn primary" }, t("common.save"));
    const bar = saveBar(save);
    save.addEventListener("click", async () => {
      save.classList.add("busy");
      try {
        const cur = await api.get("/autopilot");
        await api.put("/autopilot/settings", {
          enabled: cur.enabled, provider: prov.value, model: model.value || undefined,
          sites_per_week: Number(perWeek.value) === 0 ? -1 : Number(perWeek.value),
          zones: zones.value.split(/[\s,]+/).filter(Boolean), publish: publish.value,
          min_minutes: Number(minM.value), max_minutes: Number(maxM.value),
          quota_percent: Number(quota.value), context_percent: Number(ctx.value),
        });
        bar.saved();
      } catch (e) { toastError(e); } finally { save.classList.remove("busy"); }
    });
    const form = h("div", null,
      h("div", { class: "form-row" }, field(t("automation.provider"), prov), field(t("automation.model"), model)),
      fold(t("autopilot.brakes"), t("autopilot.brakesNote", { per: perWeek.value, quota: quota.value, ctx: ctx.value }), [
        h("div", { class: "form-row" }, field(t("autopilot.perWeek"), perWeek), field(t("autopilot.publishMode"), publish)),
        field(t("autopilot.zones"), zones, t("autopilot.zonesHint")),
        h("div", { class: "form-row" }, field(t("autopilot.minMinutes"), minM), field(t("autopilot.maxMinutes"), maxM)),
        h("div", { class: "form-row" }, field(t("autopilot.quota"), quota, t("autopilot.quotaHint")), field(t("autopilot.contextAt"), ctx, t("autopilot.contextHint"))),
      ]),
      bar.el);
    bar.watch(form);
    clear(limitsCard).append(cardHead(t("autopilot.limits"), t("autopilot.limitsHint")), form);
  }

  function renderBook(st) {
    clear(bookCard).append(cardHead(t("autopilot.notebook"), t("autopilot.notebookHint")),
      st.notebook ? h("pre", { class: "mono small pre-wrap" }, st.notebook) : h("p", { class: "hint" }, t("autopilot.notebookEmpty")));
  }

  async function renderJournal() {
    const list = (await api.get("/autopilot/journal?limit=150").catch(() => [])).slice().reverse();
    const items = list.map((e) => {
      const li = h("li", null,
        h("span", { class: "badge" }, t("autopilot.kind." + e.kind)),
        e.domain ? h("a", { href: `#/sites/${enc(e.domain)}` }, e.domain) : null,
        h("span", { class: e.kind === "refused" ? "level-warn" : "" }, e.message || ""),
        h("span", { class: "muted", title: fmtTime(e.at) }, fmtAgo(e.at, t)));
      if (e.commands && e.commands.length) {
        li.append(h("details", null, h("summary", { class: "small muted" }, t("autopilot.commands", { n: e.commands.length })),
          h("pre", { class: "mono small pre-wrap" }, e.commands.map((c) => "$ " + c).join("\n"))));
      }
      return li;
    });
    clear(journalCard).append(cardHead(t("autopilot.journal"), t("autopilot.journalHint")),
      items.length ? h("ul", { class: "set-recent-list" }, ...items) : h("p", { class: "hint" }, t("autopilot.journalEmpty")));
  }

  async function refresh() {
    const st = await api.get("/autopilot");
    renderState(st);
    renderProposals(st);
    renderSites(st);
    renderBook(st);
    return st;
  }
  load(stateCard, async () => {
    const st = await refresh();
    renderRules(st);
    await renderLimits(st);
    await renderJournal();
    return null;
  });
  let pending = null;
  const later = () => { clearTimeout(pending); pending = setTimeout(() => { refresh().catch(() => {}); renderJournal(); }, 800); };
  const subs = [
    onEvent("autopilot.", later),
    onEvent("agent.", (ev) => { if ((ev.subject || "").startsWith("autopilot#") && /^agent\.(started|done|error)$/.test(ev.type)) later(); }),
  ];
  return { el: p.el, destroy() { clearTimeout(pending); subs.forEach((u) => u()); } };
}

// Its conversations, one per session: read here, and written into by the
// operator, who can talk to it. Until it has had one there is nothing to
// show but a line saying so.
function sessionsTab(p) {
  let chat = null;
  const hint = h("div", { class: "card set-card" });
  p.body.append(hint);
  const show = async () => {
    if (chat) return;
    const list = await api.get("/autopilot/chats").catch(() => []);
    if (!list.length) {
      clear(hint).append(cardHead(t("autopilot.sessions"), t("autopilot.sessionsHint")), h("p", { class: "hint" }, t("autopilot.noSessions")));
      return;
    }
    hint.remove();
    chat = chatTab("autopilot");
    p.body.append(chat.el);
  };
  show();
  const unsub = onEvent("agent.", (ev) => { if ((ev.subject || "").startsWith("autopilot#")) show(); });
  return { el: p.el, destroy() { unsub(); if (chat) chat.destroy(); } };
}
