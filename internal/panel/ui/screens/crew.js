// The operator's own autonomous agents: the list, a new one, and each
// agent's page. An agent works with nobody watching toward the goal the
// operator wrote, so its page leads with what it waits for (its
// requests, the urgent ones first), then its switch, its goal, its
// vault, what the house gives it, its notebook and its journal. Its
// sessions are a tab of their own, where the operator talks to it.

import { icon } from "../lib/icons.js";
import { h, clear, fmtTime, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { navigate } from "../lib/router.js";
import { onEvent } from "../lib/stream.js";
import { page, load, notice } from "../components/page.js";
import { confirm, field, open } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { cardHead, saveBar, fold } from "./settings-kit.js";
import { chatTab } from "./site-chat.js";
import { tabs } from "../components/tabs.js";

function stateOf(st) {
  const a = st.agent || {}, s = st.state || {};
  if (!a.enabled) return { label: t(st.running ? "crew.state.offFinishing" : "crew.state.off"), cls: "muted" };
  if (st.running) return { label: t("crew.state.working"), cls: "level-ok" };
  if (s.skipped) return { label: t("crew.state.held"), cls: "level-warn" };
  if (s.next_wake) return { label: t("crew.state.next", { at: fmtTime(s.next_wake) }), cls: "" };
  return { label: t("crew.state.on"), cls: "level-ok" };
}

export function crewScreen({ id, tab } = {}) {
  if (id) return agentScreen(id, tab);
  const newBtn = h("button", { type: "button", class: "btn primary" }, icon("plus"), t("crew.new"));
  const p = page({ title: t("nav.crew"), sub: t("crew.sub"), icon: "bot", actions: [newBtn] });
  const card = h("div", { class: "card set-card" });
  p.body.append(card);
  newBtn.addEventListener("click", () => createAgent());
  async function render() {
    const list = await api.get("/crew");
    if (!list.length) {
      clear(card).append(cardHead(t("crew.none"), t("crew.noneHint")));
      return null;
    }
    clear(card).append(h("ul", { class: "set-recent-list" }, ...list.map((st) => {
      const s = stateOf(st);
      const waiting = (st.requests || []).filter((r) => r.status === "open");
      const urgent = waiting.some((r) => r.urgent);
      return h("li", null,
        h("a", { href: `#/agents/${enc(st.agent.id)}` }, h("strong", null, st.agent.name)),
        h("span", { class: s.cls }, s.label),
        waiting.length ? h("span", { class: "badge " + (urgent ? "level-error" : "level-warn") }, t(urgent ? "crew.urgentOpen" : "crew.requestsOpen", { n: waiting.length })) : null,
        st.state && st.state.last_result ? h("span", { class: "muted small" }, st.state.last_result) : null);
    })));
    return null;
  }
  load(card, render);
  const unsub = onEvent("crew.", () => render().catch(() => {}));
  return { el: p.el, destroy() { unsub(); } };
}

async function createAgent() {
  const name = h("input", { type: "text", maxlength: "80", placeholder: t("crew.namePlaceholder") });
  const goal = h("textarea", { rows: 10, placeholder: t("crew.goalPlaceholder") });
  const body = h("div", null, field(t("crew.name"), name), field(t("crew.goal"), goal, t("crew.goalHint")));
  return open({
    title: t("crew.new"), body, wide: true, initialFocus: name,
    buttons: [{ label: t("common.cancel"), value: null }, { label: t("crew.create"), primary: true, submit: true, value: true }],
    onSubmit: async () => {
      const st = await api.post("/crew", { name: name.value, goal: goal.value });
      navigate(`/agents/${enc(st.agent.id)}`);
      return true;
    },
  });
}

function agentScreen(id, tab) {
  const base = `/crew/${enc(id)}`;
  const p = page({ title: t("nav.crew"), sub: "", icon: "bot", crumbs: [h("a", { href: "#/agents" }, t("nav.crew"))] });
  const active = tab === "sessions" ? "sessions" : "agent";
  p.body.append(tabs([
    { key: "agent", href: `#/agents/${enc(id)}`, label: t("crew.tab.agent"), icon: "bot" },
    { key: "sessions", href: `#/agents/${enc(id)}/sessions`, label: t("crew.tab.sessions"), icon: "chat" },
  ], active));
  if (active === "sessions") return sessionsTab(p, id);
  const stateCard = h("div", { class: "card set-card" });
  const reqCard = h("div", { class: "card set-card" });
  const goalCard = h("div", { class: "card set-card" });
  const vaultCard = h("div", { class: "card set-card" });
  const capsCard = h("div", { class: "card set-card" });
  const bookCard = h("div", { class: "card set-card" });
  const journalCard = h("div", { class: "card set-card" });
  p.body.append(stateCard, reqCard, goalCard, vaultCard, capsCard, bookCard, journalCard);

  const post = (path, body, msg) => async (e) => {
    const btn = (e && e.currentTarget) || document.createElement("button");
    btn.classList.add("busy");
    try { await api.post(`${base}/${path}`, body); if (msg) toast(msg, "ok"); await refresh(); } catch (err) { toastError(err); } finally { btn.classList.remove("busy"); }
  };
  const meter = (pct) => {
    const fill = h("div", { class: "fill" + (pct >= 80 ? " bad" : pct >= 60 ? " warn" : "") });
    fill.style.width = Math.max(0, Math.min(100, pct)) + "%";
    return h("div", { class: "meter" }, fill);
  };

  function renderState(st) {
    const a = st.agent, s = st.state || {};
    const title = p.el.querySelector(".page-title h1");
    if (title) title.textContent = a.name;
    const on = a.enabled;
    const toggle = h("button", { type: "button", class: "btn" + (on ? "" : " primary") }, icon(on ? "pause" : "play"), t(on ? "crew.off" : "crew.on"));
    toggle.addEventListener("click", post(on ? "off" : "on", null, t(on ? "crew.offDone" : "crew.onDone")));
    const stop = h("button", { type: "button", class: "btn danger" }, icon("stop"), t("crew.stop"));
    stop.addEventListener("click", async (e) => {
      const ev = { currentTarget: e.currentTarget };
      if (!(await confirm({ title: t("crew.stopQ", { name: a.name }), body: t("crew.stopBody"), confirmLabel: t("crew.stop"), danger: true }))) return;
      post("stop", null, t("crew.stopped"))(ev);
    });
    const wake = h("button", { type: "button", class: "btn ghost" }, icon("refresh"), t("crew.wake"));
    wake.addEventListener("click", post("wake", null, t("crew.wakeDone")));
    const s2 = stateOf(st);
    const lines = [h("p", null, h("strong", { class: s2.cls }, s2.label))];
    if (s.skipped && on) lines.push(notice("warn", t("crew.heldBack", { why: s.skipped })));
    if (s.last_result) lines.push(h("p", { class: "small" }, t("crew.lastTurn"), " ", s.last_result));
    if (!on && !s.turns) lines.push(notice("info", t("crew.firstStart")));
    const meters = [];
    if (st.context_percent >= 0) meters.push(h("div", { class: "usage-row" }, h("div", { class: "row" }, h("span", { class: "grow" }, t("autopilot.context")), h("span", { class: "muted small" }, st.context_percent + "%")), meter(st.context_percent)));
    for (const w of st.quota || []) {
      if (w.used < 0) continue;
      meters.push(h("div", { class: "usage-row" }, h("div", { class: "row" }, h("span", { class: "grow" }, t("autopilot.window", { name: w.label || w.name })), h("span", { class: "muted small" }, t("autopilot.windowOf", { pct: Math.round(w.used), stop: a.quota_percent || 80 }))), meter(w.used)));
    }
    clear(stateCard).append(cardHead(a.name, t("crew.stateHint"), toggle, stop, wake), ...lines, ...meters);
  }

  // What it waits for, the urgent first. What the operator types into a
  // field goes to the vault, never into the request.
  function renderRequests(st) {
    const all = st.requests || [];
    const waiting = all.filter((r) => r.status === "open").sort((x, y) => Number(Boolean(y.urgent)) - Number(Boolean(x.urgent)));
    const done = all.filter((r) => r.status !== "open").slice(0, 15);
    const item = (r) => {
      const li = h("li", { class: "proposal" + (r.urgent && r.status === "open" ? " urgent" : "") },
        h("div", { class: "proposal-head" },
          h("span", { class: "badge " + (r.status === "open" ? (r.urgent ? "level-error" : "level-warn") : "muted") }, t("crew.req." + (r.status === "open" ? (r.urgent ? "urgent" : "open") : r.status === "answered" ? "answered_" : r.status))),
          h("span", { class: "muted", title: fmtTime(r.at) }, fmtAgo(r.at, t))),
        h("strong", { class: "proposal-title" }, r.title),
        r.text ? h("pre", { class: "mono small pre-wrap" }, r.text) : null);
      if (r.status !== "open") {
        if ((r.filled || []).length) li.append(h("p", { class: "small" }, t("crew.req.filled", { keys: r.filled.join(", ") })));
        if (r.choice) li.append(h("p", { class: "small" }, t("crew.req.chose", { choice: r.choice })));
        if (r.answer) li.append(h("p", { class: "small" }, r.answer));
        return li;
      }
      const inputs = (r.fields || []).map((f) => ({ f, input: h("input", { type: f.secret ? "password" : "text", autocomplete: "off", spellcheck: "false" }) }));
      let choice = "";
      const choices = (r.options || []).length ? h("div", { class: "form-row" }, ...r.options.map((o) => {
        const b = h("button", { type: "button", class: "btn sm" }, o);
        b.addEventListener("click", () => { choice = o; choices.querySelectorAll("button").forEach((x) => x.classList.toggle("primary", x === b)); });
        return b;
      })) : null;
      const words = h("textarea", { rows: 2, placeholder: t("crew.req.answerPlaceholder") });
      const send = h("button", { type: "button", class: "btn sm primary" }, icon("send"), t("crew.req.send"));
      send.addEventListener("click", (e) => {
        const values = {};
        for (const { f, input } of inputs) if (input.value) values[f.key] = input.value;
        post(`requests/${enc(r.id)}/answer`, { answer: words.value, choice, values }, t("crew.req.sent"))(e);
      });
      const drop = h("button", { type: "button", class: "btn sm ghost" }, t("crew.req.dismiss"));
      drop.addEventListener("click", (e) => post(`requests/${enc(r.id)}/answer`, { dismiss: true, answer: words.value }, null)(e));
      const talk = h("a", { class: "btn sm ghost", href: `#/agents/${enc(id)}/sessions` }, icon("chat"), t("crew.req.talk"));
      li.append(...inputs.map(({ f, input }) => field(f.key, input)), choices, field(t("crew.req.answer"), words), h("div", { class: "form-row" }, send, drop, talk));
      return li;
    };
    const parts = [cardHead(t("crew.requests"), t("crew.requestsHint"))];
    parts.push(waiting.length ? h("ul", { class: "set-recent-list proposals" }, ...waiting.map(item)) : h("p", { class: "hint" }, t("crew.req.none")));
    if (done.length) parts.push(fold(t("crew.req.answered", { n: done.length }), null, [h("ul", { class: "set-recent-list proposals" }, ...done.map(item))]));
    clear(reqCard).append(...parts);
  }

  async function renderGoal(st) {
    const a = st.agent;
    const name = h("input", { type: "text", maxlength: "80", value: a.name });
    const goal = h("textarea", { rows: 12, spellcheck: "true" });
    goal.value = a.goal || "";
    const agents = await api.get("/agents").catch(() => ({ providers: [] }));
    const providers = (agents.providers || []).filter((x) => x.available || x.provider === a.provider);
    const prov = h("select", null, ...providers.map((x) => h("option", { value: x.provider }, x.provider)));
    prov.value = a.provider || "claude";
    const model = h("select");
    const fillModels = () => {
      const x = providers.find((y) => y.provider === prov.value);
      clear(model).append(h("option", { value: "" }, t("chat.defaultModel")), ...((x && x.models) || []).map((m) => h("option", { value: m }, m)));
      model.value = (x && (x.models || []).includes(a.model)) ? a.model : "";
    };
    prov.addEventListener("change", fillModels);
    fillModels();
    const num = (v, min, max) => h("input", { type: "number", min: String(min), max: String(max), value: String(v || "") });
    const minM = num(a.min_minutes || 10, 1, 10080);
    const maxM = num(a.max_minutes || 720, 1, 10080);
    const quota = num(a.quota_percent || 80, 1, 100);
    const ctx = num(a.context_percent || 60, 10, 95);
    const save = h("button", { type: "button", class: "btn primary" }, t("common.save"));
    const bar = saveBar(save);
    save.addEventListener("click", async () => {
      save.classList.add("busy");
      try {
        await api.put(base, { name: name.value, goal: goal.value, provider: prov.value, model: model.value || undefined,
          min_minutes: Number(minM.value), max_minutes: Number(maxM.value), quota_percent: Number(quota.value), context_percent: Number(ctx.value) });
        bar.saved();
        refresh();
      } catch (e) { toastError(e); } finally { save.classList.remove("busy"); }
    });
    const del = h("button", { type: "button", class: "btn ghost danger" }, icon("trash"), t("crew.delete"));
    del.addEventListener("click", async () => {
      if (!(await confirm({ title: t("crew.deleteQ", { name: a.name }), body: t("crew.deleteBody"), confirmLabel: t("crew.delete"), danger: true }))) return;
      try { await api.del(base); navigate("/agents"); } catch (e) { toastError(e); }
    });
    bar.el.append(del);
    const form = h("div", null, field(t("crew.name"), name), field(t("crew.goal"), goal, t("crew.goalHint")),
      fold(t("crew.how"), t("crew.howNote", { quota: quota.value, ctx: ctx.value }), [
        h("div", { class: "form-row" }, field(t("automation.provider"), prov), field(t("automation.model"), model)),
        h("div", { class: "form-row" }, field(t("autopilot.minMinutes"), minM), field(t("autopilot.maxMinutes"), maxM)),
        h("div", { class: "form-row" }, field(t("autopilot.quota"), quota, t("autopilot.quotaHint")), field(t("autopilot.contextAt"), ctx, t("autopilot.contextHint"))),
      ]), bar.el);
    bar.watch(form);
    clear(goalCard).append(cardHead(t("crew.goalTitle"), t("crew.goalCardHint")), form);
  }

  function renderVault(st) {
    const keys = st.vault || [];
    const list = keys.map((k) => {
      const x = h("button", { type: "button", class: "icon-btn sm", "aria-label": t("crew.vault.remove", { key: k }), title: t("crew.vault.remove", { key: k }) }, icon("x", { size: 16 }));
      x.addEventListener("click", async () => {
        if (!(await confirm({ title: t("crew.vault.removeQ", { key: k }), body: t("crew.vault.removeBody"), confirmLabel: t("common.delete"), danger: true }))) return;
        try { await api.del(`${base}/vault/${enc(k)}`); refresh(); } catch (e) { toastError(e); }
      });
      return h("li", null, h("code", null, k), h("span", { class: "grow" }), x);
    });
    const key = h("input", { type: "text", placeholder: "SMTP_HOST", spellcheck: "false", autocomplete: "off" });
    const val = h("input", { type: "password", autocomplete: "new-password", spellcheck: "false" });
    const add = h("button", { type: "button", class: "btn" }, icon("plus"), t("crew.vault.add"));
    add.addEventListener("click", async () => {
      add.classList.add("busy");
      try { await api.put(`${base}/vault/${enc(key.value.trim().toUpperCase())}`, { value: val.value }); key.value = ""; val.value = ""; toast(t("crew.vault.saved"), "ok"); refresh(); } catch (e) { toastError(e); } finally { add.classList.remove("busy"); }
    });
    clear(vaultCard).append(cardHead(t("crew.vault"), t("crew.vaultHint")),
      list.length ? h("ul", { class: "set-recent-list" }, ...list) : h("p", { class: "hint" }, t("crew.vault.empty")),
      h("div", { class: "form-row" }, field(t("crew.vault.key"), key), field(t("crew.vault.value"), val), add));
  }

  function renderCaps(st) {
    const items = (st.capabilities || []).map((c) => h("li", null,
      h("strong", null, c.title),
      h("span", { class: c.ready ? "level-ok" : "level-warn" }, c.ready ? t("crew.cap.ready") : t("crew.cap.lacks", { keys: (c.missing || []).join(", ") }))));
    clear(capsCard).append(cardHead(t("crew.caps"), t("crew.capsHint")), h("ul", { class: "set-recent-list" }, ...items));
  }

  function renderBook(st) {
    clear(bookCard).append(cardHead(t("crew.notebook"), t("crew.notebookHint")),
      st.notebook ? h("pre", { class: "mono small pre-wrap" }, st.notebook) : h("p", { class: "hint" }, t("crew.notebookEmpty")));
  }

  async function renderJournal() {
    const list = (await api.get(`${base}/journal?limit=150`).catch(() => [])).slice().reverse();
    const items = list.map((e) => {
      const li = h("li", null,
        h("span", { class: "badge" }, e.kind),
        h("span", { class: e.kind === "refused" ? "level-warn" : "" }, e.message || ""),
        h("span", { class: "muted", title: fmtTime(e.at) }, fmtAgo(e.at, t)));
      if (e.commands && e.commands.length) {
        li.append(h("details", null, h("summary", { class: "small muted" }, t("autopilot.commands", { n: e.commands.length })),
          h("pre", { class: "mono small pre-wrap" }, e.commands.map((c) => "$ " + c).join("\n"))));
      }
      return li;
    });
    clear(journalCard).append(cardHead(t("crew.journal"), t("crew.journalHint")),
      items.length ? h("ul", { class: "set-recent-list" }, ...items) : h("p", { class: "hint" }, t("crew.journalEmpty")));
  }

  async function refresh() {
    const st = await api.get(base);
    renderState(st);
    renderRequests(st);
    renderVault(st);
    renderCaps(st);
    renderBook(st);
    return st;
  }
  load(stateCard, async () => {
    const st = await refresh();
    await renderGoal(st);
    await renderJournal();
    return null;
  });
  let pending = null;
  const later = () => { clearTimeout(pending); pending = setTimeout(() => { refresh().catch(() => {}); renderJournal(); }, 800); };
  const subs = [
    onEvent("crew.", (ev) => { if (ev.subject === id) later(); }),
    onEvent("agent.", (ev) => { if ((ev.subject || "").startsWith(`crew~${id}#`) && /^agent\.(started|done|error)$/.test(ev.type)) later(); }),
  ];
  return { el: p.el, destroy() { clearTimeout(pending); subs.forEach((u) => u()); } };
}

// Its sessions: read here, and written into by the operator, who talks
// to it. Until it has had one there is a line saying so.
function sessionsTab(p, id) {
  let chat = null;
  const hint = h("div", { class: "card set-card" });
  p.body.append(hint);
  const show = async () => {
    if (chat) return;
    const list = await api.get(`/crew/${enc(id)}/chats`).catch(() => []);
    if (!list.length) {
      clear(hint).append(cardHead(t("crew.tab.sessions"), t("crew.sessionsHint")), h("p", { class: "hint" }, t("crew.noSessions")));
      return;
    }
    hint.remove();
    chat = chatTab(`crew~${id}`);
    p.body.append(chat.el);
  };
  show();
  const unsub = onEvent("agent.", (ev) => { if ((ev.subject || "").startsWith(`crew~${id}#`)) show(); });
  return { el: p.el, destroy() { unsub(); if (chat) chat.destroy(); } };
}
