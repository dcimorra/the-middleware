// The house's requirements on every node at once: one press installs
// what is missing on each machine of the network (POST
// /system/requirements on each, through the mesh), each node's run is
// followed by its steps, and the result is checked rather than assumed: a
// run that ended says which steps did not go well, and a node is only
// "checked" once `middleware doctor` passes on it. A restart is never
// done for the operator: once every run has ended, each node that needs
// one gets its own button and its own confirmation, this panel's node
// last, since restarting it takes the panel away for a moment.
//
// Hidden on a node alone: the card beside it already does that.

import { icon } from "../lib/icons.js";
import { h, clear } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api } from "../lib/api.js";
import { confirm, checkbox } from "../components/dialog.js";
import { toastError } from "../components/toast.js";
import { checklist } from "../components/checklist.js";
import { cardHead } from "./settings-kit.js";

const BADGE = { running: "info", finished: "ok", restart: "warn", failed: "bad", interrupted: "bad", unreachable: "bad", restarting: "info", none: "" };

// How long a restarted node has to answer again before it is said as not
// back: a supervisor restart re-adopts every site, which takes seconds.
const BACK_WITHIN = 3 * 60 * 1000;

export function requirementsAllCard() {
  const el = h("div", { class: "card sys-card", hidden: true });
  const browser = checkbox(t("reqAll.browser"));
  const runBtn = h("button", { type: "button", class: "btn primary" }, icon("download"), t("reqAll.run"));
  const checkBtn = h("button", { type: "button", class: "btn" }, icon("check"), t("reqAll.check"));
  const verdict = h("div", { class: "sys-req-state" });
  const list = h("div", { class: "req-all" });
  el.append(cardHead(t("reqAll.title"), t("reqAll.hint")), verdict, list, h("div", { class: "field" }, browser.el), h("div", { class: "form-actions sys-foot" }, runBtn, checkBtn));

  // One entry per node: what its run says (st), whether it answered,
  // what was asked of it here (asked, restarting) and its doctor's answer.
  let nodes = [];
  let timer = null;
  let gone = false;
  const at = (n) => api.at(n.self ? "" : n.id);

  async function loadNode(n) {
    try {
      n.st = await at(n).get("/system/requirements");
      n.error = "";
    } catch (e) {
      n.st = null;
      n.error = e.message || String(e);
    }
  }

  // A run just asked for has no log yet for a moment, and reads "none":
  // still running, or the polling would stop before it starts.
  const stateOf = (n) => {
    if (n.restarting) return "restarting";
    if (!n.st) return "unreachable";
    const s = n.st.state || "none";
    if (s === "none" && n.askedAt && Date.now() - n.askedAt < 60000) return "running";
    return s;
  };
  const badSteps = (n) => ((n.st && n.st.steps) || []).filter((s) => !s.ok);
  const running = () => nodes.some((n) => stateOf(n) === "running" || n.restarting);

  function paintVerdict() {
    clear(verdict);
    const asked = nodes.filter((n) => n.asked);
    if (!asked.length || running()) {
      if (running()) verdict.append(h("span", { class: "badge info" }, t("reqAll.inProgress", { n: nodes.filter((n) => stateOf(n) === "running").length })));
      return;
    }
    const wrong = asked.filter((n) => !["finished", "restart"].includes(stateOf(n)) || badSteps(n).length);
    const restart = nodes.filter((n) => stateOf(n) === "restart");
    if (wrong.length) {
      verdict.append(h("span", { class: "badge bad" }, t("reqAll.wrong", { nodes: wrong.map((n) => n.name).join(", ") })));
    } else {
      verdict.append(h("span", { class: "badge ok" }, t("reqAll.allOk", { n: asked.length })));
    }
    if (restart.length) verdict.append(h("span", { class: "small" }, t("reqAll.restartNext", { n: restart.length })));
  }

  function row(n) {
    const state = stateOf(n);
    const label = state === "unreachable" ? t("reqAll.unreachable") : state === "restarting" ? t("reqAll.restarting") : (state === "none" ? t("reqAll.none") : t("req.state." + state));
    const head = h("div", { class: "req-all-head" },
      h("strong", null, n.name || n.id),
      n.self ? h("span", { class: "small muted" }, t("reqAll.thisNode")) : null,
      state !== "none" || n.asked ? h("span", { class: "badge " + (BADGE[state] || "") }, label) : null);
    const body = h("div", { class: "req-all-body" });
    if (n.error && !n.restarting) body.append(h("div", { class: "small level-error" }, n.error));
    if (n.st && n.st.error) body.append(h("div", { class: "small level-error" }, n.st.error));
    const steps = (n.st && n.st.steps) || [];
    const bad = badSteps(n);
    if (steps.length && !bad.length && state !== "running") body.append(h("div", { class: "small muted" }, t("reqAll.stepsOk", { n: steps.length })));
    if (bad.length) body.append(checklist(bad.map((s) => ({ state: s.optional ? "skip" : "fail", text: s.name + (s.detail ? ": " + s.detail : "") }))).el);
    if (n.backError) body.append(h("div", { class: "small level-error" }, n.backError));
    if (n.doctor) {
      const fails = (n.doctor.checks || []).filter((c) => !c.ok);
      body.append(h("div", null, h("span", { class: "badge " + (n.doctor.ok ? "ok" : "bad") }, t(n.doctor.ok ? "reqAll.checked" : "doctor.someFail"))));
      if (fails.length) body.append(checklist(fails.map((c) => ({ ok: false, text: c.name + ": " + c.detail }))).el);
    }
    const actions = h("div", { class: "req-all-actions" });
    if (n.st && n.st.log) {
      const logBtn = h("button", { type: "button", class: "btn sm" }, icon("list"), t("req.seeLog"));
      logBtn.addEventListener("click", () => openLog(n));
      actions.append(logBtn);
    }
    // Only once nothing is installing anywhere, so a restart never lands
    // on a node while another is still halfway, and the operator decides
    // node by node.
    if (state === "restart" && !running()) {
      const others = nodes.filter((o) => o !== n && stateOf(o) === "restart");
      const restartBtn = h("button", { type: "button", class: "btn sm" + (n.self && others.length ? "" : " primary") }, icon("power"), t("reqAll.restartNode"));
      restartBtn.addEventListener("click", () => restartNode(n));
      actions.append(restartBtn);
    }
    return h("div", { class: "req-all-row" }, head, body, actions.childElementCount ? actions : null);
  }

  function paint() {
    // The node this panel is on goes last: it is the one to restart last.
    const order = [...nodes.filter((n) => !n.self), ...nodes.filter((n) => n.self)];
    clear(list).append(...order.map(row));
    paintVerdict();
    runBtn.disabled = running();
    checkBtn.disabled = running();
  }

  function schedule() {
    clearTimeout(timer);
    if (gone || !running()) return;
    timer = setTimeout(async () => {
      await Promise.all(nodes.filter((n) => stateOf(n) === "running").map(loadNode));
      paint();
      schedule();
    }, 2000);
  }

  async function doctor(n) {
    try { n.doctor = await at(n).get("/system/doctor"); } catch (e) { n.doctor = { ok: false, checks: [{ name: "doctor", ok: false, detail: e.message || String(e) }] }; }
  }

  async function restartNode(n) {
    const body = n.self ? t("reqAll.restartSelfBody") : t("reqAll.restartBody", { node: n.name });
    if (!(await confirm({ title: t("reqAll.restartQ", { node: n.name }), body, confirmLabel: t("reqAll.restartNode"), danger: true }))) return;
    try {
      await at(n).post("/system/restart");
    } catch (e) { toastError(e); return; }
    n.restarting = true;
    n.doctor = null;
    n.backError = "";
    paint();
    // Back when its run reads "finished": the requirements say so only to
    // a supervisor started after the run ended. Then the doctor checks it.
    const until = Date.now() + BACK_WITHIN;
    await new Promise((r) => setTimeout(r, 1500));
    while (!gone && Date.now() < until) {
      await loadNode(n);
      if (n.st && n.st.state === "finished") break;
      await new Promise((r) => setTimeout(r, 2000));
    }
    n.restarting = false;
    if (!n.st || n.st.state !== "finished") n.backError = t("reqAll.notBack");
    else await doctor(n);
    paint();
  }

  function openLog(n) {
    const pre = h("pre", { class: "req-log" }, (n.st && n.st.log) || "");
    const close = h("button", { type: "button", class: "btn" }, t("moves.hide"));
    const dlg = h("dialog", { class: "wide" }, h("div", { class: "dlg" },
      h("div", { class: "dlg-head" }, h("h2", null, t("req.title") + " · " + (n.name || n.id))),
      h("div", { class: "dlg-body" }, pre),
      h("div", { class: "dlg-foot" }, close)));
    document.body.append(dlg);
    const end = () => { dlg.close(); dlg.remove(); };
    close.addEventListener("click", end);
    dlg.addEventListener("cancel", (e) => { e.preventDefault(); end(); });
    dlg.showModal();
    pre.scrollTop = pre.scrollHeight;
  }

  runBtn.addEventListener("click", async () => {
    if (!(await confirm({ title: t("reqAll.runQ", { n: nodes.length }), body: t("reqAll.runBody"), confirmLabel: t("reqAll.run") }))) return;
    runBtn.classList.add("busy");
    await Promise.all(nodes.map(async (n) => {
      n.asked = true;
      n.askedAt = Date.now();
      n.doctor = null;
      n.backError = "";
      if (stateOf(n) === "running") return; // already at it: followed, not asked twice
      try {
        await at(n).post("/system/requirements", { browser: browser.input.checked });
        n.st = { state: "running", steps: [] };
        n.error = "";
      } catch (e) {
        n.askedAt = 0;
        n.error = e.message || String(e);
        await loadNode(n);
        if (!n.error) n.error = e.message || String(e);
      }
    }));
    runBtn.classList.remove("busy");
    paint();
    schedule();
  });

  checkBtn.addEventListener("click", async () => {
    checkBtn.classList.add("busy");
    await Promise.all(nodes.map(async (n) => { await loadNode(n); await doctor(n); }));
    checkBtn.classList.remove("busy");
    paint();
  });

  async function load() {
    let cards;
    try { cards = await api.get("/nodes/overview", { node: "" }); } catch { return; }
    if (!Array.isArray(cards) || cards.length < 2) return;
    nodes = cards.map((c) => ({ id: c.id, name: c.name || c.id, self: !!c.self, st: null, error: c.ok ? "" : c.error || "" }));
    await Promise.all(nodes.map(loadNode));
    el.hidden = false;
    paint();
    schedule();
  }
  load();
  return { el, destroy() { gone = true; clearTimeout(timer); } };
}
