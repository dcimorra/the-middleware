// The moves of sites from this node to others, as their operations: each
// step with its state, how long it took and, when it failed, why; and the
// way back when there was one. Read from GET /moves, which keeps them on
// disk, and redrawn on every relocate.* event, so a move goes on in the
// background and is read here whenever, also once the site has left this
// node and its own page is gone.

import { h, clear, fmtAgo, fmtDuration, fmtTime, debounce } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { onEvent } from "../lib/stream.js";
import { icon } from "../lib/icons.js";

const BADGE = { running: "info", ok: "ok", failed: "bad", warn: "warn" };

const secs = (a, b) => (new Date(b || Date.now()) - new Date(a)) / 1000;

function jobState(j) {
  if (!j.done) return "running";
  if (j.error) return "failed";
  return (j.steps || []).some((s) => s.state === "warn") ? "warn" : "ok";
}

function stepRow(j, s) {
  return h("li", { class: "move-step " + s.state },
    h("span", { class: "badge " + (BADGE[s.state] || "") }, t("moves.state." + s.state)),
    h("div", { class: "grow" },
      h("div", null, t("site.moveStep." + s.step, { node: j.node })),
      // The install says what it did (the tool, its last line, how many
      // programs in node_modules/.bin): an install that leaves out what the
      // start needs looks done otherwise. The other steps' lines only
      // repeat their label.
      s.step === "install" && s.state !== "running" && s.message ? h("div", { class: "small dim move-error" }, s.message) : null,
      s.error ? h("div", { class: "small level-error move-error" }, s.error) : null),
    h("span", { class: "small muted mono", title: fmtTime(s.started) }, fmtDuration(secs(s.started, s.ended))));
}

function jobEl(j, open) {
  const state = jobState(j);
  const head = h("summary", { class: "row" },
    h("span", { class: "badge " + BADGE[state] }, t("moves.state." + state)),
    h("strong", null, t("moves.title1", { domain: j.domain, node: j.node })),
    h("span", { class: "grow" }),
    h("span", { class: "small muted", title: fmtTime(j.started) }, fmtAgo(j.started, t) + " · " + fmtDuration(secs(j.started, j.ended))));
  const body = h("div", { class: "stack-sm" },
    h("ol", { class: "move-steps" }, ...(j.steps || []).map((s) => stepRow(j, s))),
    // The failed step already says why; the job's own error is said only
    // when no step carries it (one cut before its first step).
    j.error && !(j.steps || []).some((s) => s.error) ? h("div", { class: "notice bad small move-error" }, j.error) : null,
    downFor(j) && state !== "failed" ? h("p", { class: "small muted" }, t("moves.down", { down: downFor(j) })) : null);
  return h("details", { class: "move-job", open: open || state === "running" }, head, body);
}

// movesCard is the card for every move (domain empty) or one site's. It
// hides itself while there is none.
export function movesCard(domain, opts = {}) {
  const list = h("div", { class: "stack-sm" });
  const el = h("div", { class: "card", hidden: true }, h("h2", null, t(domain ? "moves.siteTitle" : "moves.title")), h("p", { class: "small muted" }, t("moves.hint")), list);
  let seen = false;
  async function load() {
    let jobs = [];
    try { jobs = await api.get("/moves" + (domain ? "?domain=" + enc(domain) : "")); } catch { jobs = []; }
    if (!domain) jobs = jobs.slice(0, 10);
    el.hidden = !jobs.length && !opts.empty;
    // The newest open the first time; after that, what the operator opened stays.
    const opened = new Set([...list.querySelectorAll("details[open]")].map((d) => d.dataset.id));
    clear(list).append(...jobs.map((j, i) => {
      const d = jobEl(j, seen ? opened.has(j.id) : i === 0);
      d.dataset.id = j.id;
      return d;
    }));
    if (!jobs.length && opts.empty) list.append(h("p", { class: "small muted" }, t("moves.none")));
    seen = true;
  }
  const reload = debounce(load, 300);
  const off = onEvent("relocate.", (ev) => { if (!domain || ev.subject === domain) reload(); });
  // A running step's clock moves without events.
  const timer = setInterval(() => { if (!document.hidden && list.querySelector(".badge.info")) load(); }, 5000);
  load();
  return { el, reload: load, destroy() { off(); clearInterval(timer); } };
}

// progress is how far a move is, 0 to 100, by the steps of its plan: a
// step done counts whole, the one running half. The copy is the long one,
// so the bar moves unevenly; what it never does is say more than happened.
function progress(j) {
  const plan = j.plan && j.plan.length ? j.plan : null;
  if (!plan) return j.done ? 100 : 0;
  if (j.done && !j.error) return 100;
  let n = 0;
  for (const s of j.steps || []) {
    if (!plan.includes(s.step)) continue;
    n += s.state === "running" ? 0.5 : s.state === "failed" ? 0 : 1;
  }
  return Math.min(100, Math.round((n / plan.length) * 100));
}

async function fetchMove(domain, id) {
  const list = await api.get("/moves?domain=" + enc(domain));
  return (id && list.find((j) => j.id === id)) || list[0] || null;
}

// downFor is how long the site was down, said: the report carries it as
// Go's duration, in nanoseconds (4000000000 is four seconds, which is what
// the first version of this dialog printed).
function downFor(j) {
  const ns = j.report && j.report.down;
  return ns ? fmtDuration(ns / 1e9) || "0s" : "";
}

// timeline is every step of the plan, in order: done, running, failed,
// still to come, and the way back when there was one.
function timeline(j) {
  const plan = j.plan && j.plan.length ? j.plan : (j.steps || []).map((s) => s.step);
  const byStep = new Map((j.steps || []).map((s) => [s.step, s]));
  const names = [...plan];
  for (const s of j.steps || []) if (!names.includes(s.step)) names.push(s.step);
  const failedAt = (j.steps || []).findIndex((s) => s.state === "failed");
  return names.map((name) => {
    const s = byStep.get(name);
    // After a failure the rest of the plan will not happen: not "pending".
    const state = s ? s.state : (j.done ? "skipped" : "pending");
    const mark = { ok: icon("check", { size: 14, stroke: 2.5 }), failed: icon("x", { size: 14, stroke: 2.5 }), running: h("span", { class: "spin-dot" }), warn: icon("alert", { size: 14 }), skipped: icon("minus", { size: 14 }), pending: null }[state];
    return h("li", { class: "tl-step " + state },
      h("span", { class: "tl-mark", "aria-hidden": "true" }, mark),
      h("div", { class: "tl-body" },
        h("div", { class: "tl-label" }, t("site.moveStep." + name, { node: j.node }),
          h("span", { class: "tl-time mono" }, s ? fmtDuration(secs(s.started, s.ended)) : "")),
        s && (name === "install" || name === "repair") && s.state !== "running" && s.message ? h("div", { class: "tl-note" }, s.message) : null,
        s && s.error ? h("div", { class: "tl-error" }, s.error) : null));
  });
}

// openMove shows one move in a dialog: where it goes, the bar with the
// step it is in, the outcome when it ends, and "See details" with the
// whole plan as a timeline. Closing it does not stop anything: the move
// goes on on the server, and the pill brings the dialog back.
export function openMove(domain, id) {
  const bar = h("div", { class: "move-bar-fill" });
  const pctEl = h("span", { class: "move-pct mono" });
  const now = h("div", { class: "move-now" });
  const meta = h("div", { class: "move-meta" });
  const outcome = h("div");
  const steps = h("ol", { class: "tl" });
  const more = h("details", { class: "move-more" }, h("summary", null, icon("chevronRight", { size: 16 }), t("moves.details")), steps);
  const pill = h("span", { class: "badge" });
  const title = h("h2", null, domain);
  const sub = h("div", { class: "move-sub" });
  const close = h("button", { type: "button", class: "btn", autofocus: true }, t("moves.hide"));
  const dlg = h("dialog", { class: "move-dialog" }, h("div", { class: "dlg" },
    h("div", { class: "move-head" }, h("div", { class: "move-title" }, title, sub), pill),
    h("div", { class: "dlg-body stack-sm" },
      h("div", { class: "move-progress" }, h("div", { class: "move-bar", role: "progressbar", "aria-valuemin": "0", "aria-valuemax": "100" }, bar), pctEl),
      h("div", { class: "move-status" }, now, meta),
      outcome, more),
    h("div", { class: "dlg-foot" }, h("span", { class: "small muted grow" }, t("moves.background")), close)));
  document.body.append(dlg);
  let off = () => {};
  const timer = setInterval(() => { if (!document.hidden) load(); }, 5000);
  const done = () => { off(); clearInterval(timer); dlg.close(); dlg.remove(); };
  close.addEventListener("click", done);
  dlg.addEventListener("cancel", (e) => { e.preventDefault(); done(); });
  async function load() {
    let j;
    try { j = await fetchMove(domain, id); } catch (e) { now.textContent = e.message; return; }
    if (!j) { now.textContent = t("moves.none"); return; }
    id = j.id;
    const state = jobState(j);
    const p = progress(j);
    title.textContent = j.domain;
    clear(sub).append(icon("upload", { size: 14 }), t("moves.to", { node: j.node }));
    pill.className = "badge " + BADGE[state];
    pill.textContent = t("moves.state." + state);
    dlg.dataset.state = state;
    bar.style.width = p + "%";
    bar.parentElement.setAttribute("aria-valuenow", String(p));
    pctEl.textContent = p + "%";
    const plan = j.plan && j.plan.length ? j.plan : [];
    const doneN = (j.steps || []).filter((s) => plan.includes(s.step) && s.state === "ok").length;
    const last = (j.steps || [])[j.steps.length - 1];
    now.textContent = state === "running" ? (last ? t("site.moveStep." + last.step, { node: j.node }) : t("moves.starting")) : t("moves.ended." + state);
    meta.textContent = (plan.length ? t("moves.stepOf", { n: Math.min(doneN + (state === "running" ? 1 : 0), plan.length), of: plan.length }) + " · " : "") + fmtDuration(secs(j.started, j.ended));
    clear(outcome);
    if (state === "ok") outcome.append(h("div", { class: "notice ok" }, icon("check", { size: 16 }), " ", t("moves.okSay", { domain: j.domain, node: j.node }), downFor(j) ? " " + t("moves.down", { down: downFor(j) }) : ""));
    if (state === "warn") outcome.append(h("div", { class: "notice warn" }, icon("alert", { size: 16 }), " ", t("moves.warnSay")));
    if (state === "failed") {
      const back = (j.steps || []).some((s) => s.step === "restore" && s.state === "ok");
      outcome.append(h("div", { class: "notice bad" }, icon("alert", { size: 16 }), " ", t(back ? "moves.failedBack" : "moves.failedSay")));
      more.open = true;
    }
    clear(steps).append(...timeline(j));
  }
  off = onEvent("relocate.", (ev) => { if (ev.subject === domain) load(); });
  dlg.showModal();
  close.focus();
  load();
}

// movesPill is the move in course, wherever the panel is: a button at the
// bottom that opens its dialog, after a reload too, for as long as it
// runs, and once with how it ended. Nothing shows without moves (or on a
// node without a network, where /moves does not exist).
export function movesPill() {
  const el = h("div", { class: "move-pills", hidden: true });
  const shown = new Map(); // id → the job last drawn
  const dismissed = new Set();
  async function load() {
    let jobs = [];
    try { jobs = await api.get("/moves"); } catch { jobs = []; }
    for (const j of jobs) {
      if (!j.done || shown.has(j.id)) shown.set(j.id, j);
    }
    clear(el);
    for (const j of shown.values()) {
      if (dismissed.has(j.id)) continue;
      const state = jobState(j);
      const pill = h("button", { type: "button", class: "move-pill " + state },
        h("span", { class: "badge " + BADGE[state] }, state === "running" ? progress(j) + "%" : t("moves.state." + state)),
        h("span", null, t("moves.title1", { domain: j.domain, node: j.node })));
      pill.addEventListener("click", () => {
        if (j.done) { dismissed.add(j.id); load(); }
        openMove(j.domain, j.id);
      });
      el.append(pill);
    }
    el.hidden = !el.children.length;
  }
  const reload = debounce(load, 300);
  onEvent("relocate.", reload);
  setInterval(() => { if (!document.hidden && [...shown.values()].some((j) => !j.done)) load(); }, 10000);
  return { el, load };
}
