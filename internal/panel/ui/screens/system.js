// System: the process itself. Numbers refresh on middleware.* events and
// every 10s while open, because load and memory do not emit events and a
// stale number on this screen is what an operator comes here to check.

import { icon } from "../lib/icons.js";
import { vitalsCard } from "./system-vitals.js";
import { requirementsCard } from "./system-requirements.js";
import { requirementsAllCard } from "./system-requirements-all.js";
import { doctorCard } from "./system-doctor.js";
import { leftoversCard } from "./system-leftovers.js";
import { h, clear, append, fmtBytes, fmtDuration } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { onEvent } from "../lib/stream.js";
import { page, load, table, cell, empty, kv, notice } from "../components/page.js";
import { confirm, toggle, field } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { cardHead, fold } from "./settings-kit.js";

export function systemScreen() {
  const restartBtn = h("button", { type: "button", class: "btn danger" }, icon("power"), t("system.restart"));
  const p = page({ title: t("nav.system"), icon: "cpu", actions: restartBtn });
  const stats = h("div", { class: "sys-stats" });
  const info = h("div", { class: "card sys-card" });
  const procs = h("div", { class: "card pad0 sys-card" });
  const portsCard = h("div", { class: "card sys-card" });
  const updateCard = h("div", { class: "card sys-card" });
  const vitals = vitalsCard();
  const reqs = requirementsCard();
  const reqsAll = requirementsAllCard();
  const doctor = doctorCard();
  const leftovers = leftoversCard();
  // In the order an operator comes for them: how the machine is now, the
  // version and the process, what runs, and the tools for fixing the
  // house last. Nine cards stacked in one column had no order at all.
  const section = (key, ...cards) => h("section", { class: "sys-section" }, h("h3", { class: "sys-section-h" }, t("system.section." + key)), ...cards);
  p.body.append(
    stats, vitals.el,
    section("software", h("div", { class: "sys-grid" }, updateCard, info)),
    section("running", procs, portsCard),
    section("care", reqsAll.el, h("div", { class: "sys-grid sys-grid-3" }, doctor.el, reqs.el, leftovers.el)),
  );
  const subs = [];
  let timer = null;

  // A number with its line under it and, for what fills up, a bar: "1.3
  // GB / 16 GB (8%)" in one large line wrapped into three on a phone.
  function stat(label, value, sub, fill) {
    const tile = h("div", { class: "sys-stat" }, h("div", { class: "sys-stat-label" }, label), h("div", { class: "sys-stat-value" }, value));
    if (fill !== undefined && fill !== null) {
      const bar = h("div", { class: "dash-bar-fill" + (fill >= 90 ? " bad" : fill >= 75 ? " warn" : "") });
      bar.style.width = Math.min(100, Math.max(0, fill)).toFixed(0) + "%";
      tile.append(h("div", { class: "dash-bar sys-stat-bar" }, bar));
    }
    if (sub) tile.append(h("div", { class: "sys-stat-sub" }, sub));
    return tile;
  }
  const share = (part, total) => (part !== undefined && total ? (part / total) * 100 : null);

  async function render() {
    const s = await api.get("/system");
    const mem = s.memory || {};
    const disk = s.disk || {};
    // load is {"1m","5m","15m"} from the API; older builds sent an array.
    const l = s.load;
    const loads = Array.isArray(l) ? l.map((x) => Number(x).toFixed(2))
      : l && typeof l === "object" ? ["1m", "5m", "15m"].map((k) => Number(l[k] ?? 0).toFixed(2))
      : [String(l ?? "")];
    // uptime arrives already formatted ("31s", "3h12m"); a number is seconds.
    const uptime = typeof s.uptime === "number" ? fmtDuration(s.uptime) : (s.uptime || (s.started ? fmtDuration((Date.now() - new Date(s.started)) / 1000) : ""));
    const memUsed = mem.used ?? (mem.total && mem.free ? mem.total - mem.free : undefined);
    const diskUsed = disk.used ?? (disk.total && disk.free ? disk.total - disk.free : undefined);
    const memPct = share(memUsed, mem.total), diskPct = share(diskUsed, disk.total);
    clear(stats).append(
      stat(t("system.version"), h("span", { class: "mono" }, s.version || ""), uptime ? t("system.upFor", { t: uptime }) : ""),
      stat(t("system.load"), loads[0], loads.length === 3 ? t("system.loadMore", { m5: loads[1], m15: loads[2] }) : ""),
      stat(t("system.memory"), memPct !== null ? `${memPct.toFixed(0)}%` : fmtBytes(mem.total), memPct !== null ? t("system.ofTotal", { used: fmtBytes(memUsed), total: fmtBytes(mem.total) }) : "", memPct),
      stat(t("system.disk"), diskPct !== null ? `${diskPct.toFixed(0)}%` : fmtBytes(disk.total), diskPct !== null ? t("system.ofTotal", { used: fmtBytes(diskUsed), total: fmtBytes(disk.total) }) : "", diskPct),
    );
    clear(info).append(cardHead(t("system.process"), t("system.processHint")), kv([
      [t("system.root"), h("code", null, s.root || "")],
      [t("system.listen"), h("code", null, s.listen || "")],
      [t("settings.panelDomain"), s.panel_domain || h("span", { class: "muted" }, t("system.loopbackOnly"))],
      [t("system.go"), s.go || ""],
    ]));
    return null;
  }

  // The first few processes, and the rest on demand: a house with a
  // site's forty children put the tools for fixing the house two screens
  // below. Kept across the ten-second refresh once opened.
  const FEW = 10;
  let allProcs = false;
  async function renderProcs() {
    const all = (await api.get("/system/processes")) || [];
    const list = allProcs ? all : all.slice(0, FEW);
    const rows = (list || []).map((pr) => h("tr", null,
      cell(String(pr.pid), t("system.col.pid"), "mono num proc-pid"),
      cell(pr.domain ? h("a", { href: `#/sites/${enc(pr.domain)}` }, pr.domain) : "—", null, "primary"),
      cell(pr.kind || "", t("system.col.kind")),
      cell(fmtBytes(pr.rss), t("system.col.rss"), "num mono"),
      cell(pr.cpu !== undefined ? Number(pr.cpu).toFixed(1) + "%" : "", t("system.col.cpu"), "num mono"),
      cell(pr.started ? fmtDuration((Date.now() - new Date(pr.started)) / 1000) : "", t("system.col.age"), "num"),
    ));
    const tb = rows.length ? table([{ label: t("system.col.pid"), class: "num" }, { label: t("system.col.domain") }, { label: t("system.col.kind") }, { label: t("system.col.rss"), class: "num" }, { label: t("system.col.cpu"), class: "num" }, { label: t("system.col.age"), class: "num" }], rows, { responsive: true }) : null;
    if (tb) tb.el.classList.add("proc-table");
    let more = null;
    if (all.length > FEW) {
      more = h("button", { type: "button", class: "btn ghost sm sys-more" }, allProcs ? t("system.fewerProcesses") : t("system.allProcesses", { n: all.length }));
      more.addEventListener("click", () => { allProcs = !allProcs; renderProcs().catch(() => {}); });
    }
    clear(procs).append(...[
      h("div", { class: "card-pad" }, cardHead(t("system.processes"), t("system.processCount", { n: all.length }))),
      tb ? tb.el : empty(t("system.noProcesses"), t("system.noProcessesHint")),
      more ? h("div", { class: "sys-more-row" }, more) : null,
    ].filter(Boolean));
    return null;
  }

  async function renderPorts() {
    const pt = await api.get("/system/ports");
    const used = Object.entries(pt.in_use || {}).sort((a, b) => Number(a[0]) - Number(b[0]));
    append(clear(portsCard), [
      cardHead(t("system.ports"), t("system.portsRange", { from: pt.from, to: pt.to, n: used.length })),
      used.length ? h("div", { class: "sys-ports" }, ...used.map(([port, domain]) => h("span", { class: "sys-port" }, h("code", null, port), h("span", null, domain)))) : null,
    ]);
    return null;
  }

  // The self-update: a check every so many minutes, chosen here; what it
  // finds installs itself only with the switch on, and a button for now.
  // It asks GitHub when the screen opens (at most hourly, the server keeps
  // it).
  async function renderUpdate(check) {
    const u = check === "force" ? await api.post("/system/update/check") : await api.get(`/system/update${check ? "?check=1" : ""}`);
    const auto = toggle(t("system.update.auto"), { checked: u.auto });
    auto.input.addEventListener("change", async () => {
      try {
        const cur = await api.get("/settings");
        await api.put("/settings", { ...cur, update: { ...(cur.update || {}), auto: auto.input.checked } });
        toast(t(auto.input.checked ? "system.update.autoOn" : "system.update.autoOff"), "ok");
      } catch (e) { auto.input.checked = !auto.input.checked; toastError(e); }
    });
    const every = h("input", { type: "number", min: "1", max: "10080", step: "1", inputmode: "numeric", value: String(u.every_minutes || 60), class: "num-input" });
    every.addEventListener("change", async () => {
      const n = Number(every.value);
      if (!Number.isInteger(n) || n < 1 || n > 10080) { toast(t("system.update.everyBad"), "bad"); every.value = String(u.every_minutes || 60); return; }
      try {
        const cur = await api.get("/settings");
        await api.put("/settings", { ...cur, update: { ...(cur.update || {}), every_minutes: n } });
        toast(t("system.update.everySaved", { n }), "ok");
      } catch (e) { toastError(e); }
    });
    const checkBtn = h("button", { type: "button", class: "btn" }, icon("refresh"), t("system.update.check"));
    checkBtn.addEventListener("click", async () => {
      checkBtn.classList.add("busy");
      try { await renderUpdate("force"); } catch (e) { toastError(e); } finally { checkBtn.classList.remove("busy"); }
    });
    // Update now is the primary action, and only there when there is
    // something to install: a pale disabled blue button said "do this"
    // to a build that cannot update at all.
    const installBtn = h("button", { type: "button", class: "btn primary", hidden: !u.available, disabled: !(u.available && u.can_install) || u.installing }, icon("download"), t("system.update.install"));
    installBtn.addEventListener("click", async () => {
      if (!(await confirm({ title: t("system.update.installQ", { from: u.current, to: u.latest }), body: t("system.update.installBody"), confirmLabel: t("system.update.install") }))) return;
      installBtn.classList.add("busy");
      installBtn.disabled = true;
      try { const r = await api.post("/system/update/install"); toast(t("system.update.installed", { v: r.version }), "ok"); }
      catch (e) { toastError(e); installBtn.disabled = false; }
      finally { installBtn.classList.remove("busy"); }
    });
    // One line says where the house stands, coloured by it; the times of
    // the checks are the quiet line under it.
    let state;
    if (u.error) state = h("div", { class: "sys-update-state warn" }, icon("alert", { size: 18 }), h("span", null, t("system.update.error", { error: u.error })));
    else if (!u.checked_at) state = h("div", { class: "sys-update-state" }, icon("info", { size: 18 }), h("span", null, t("system.update.notChecked")));
    else if (u.available) state = h("div", { class: "sys-update-state ok" }, icon("download", { size: 18 }), h("span", null, t("system.update.available", { v: u.latest })));
    else state = h("div", { class: "sys-update-state ok" }, icon("check", { size: 18 }), h("span", null, t("system.update.current")));
    const when = (iso) => (iso && !iso.startsWith("0001") ? new Date(iso).toLocaleString(undefined, { dateStyle: "short", timeStyle: "short" }) : "—");
    append(clear(updateCard), [
      cardHead(t("system.update.title"), null),
      h("div", { class: "sys-versions" },
        h("div", null, h("span", { class: "sys-v-label" }, t("system.update.running")), h("code", { class: "sys-v" }, u.current || "—")),
        h("div", null, h("span", { class: "sys-v-label" }, t("system.update.latest")), h("code", { class: "sys-v" }, u.latest || "—"))),
      state,
      u.can_install ? null : h("p", { class: "sys-why" }, t("system.update.why." + u.why)),
      h("p", { class: "sys-checks" }, t("system.update.checks", { last: when(u.checked_at), next: when(u.next_check) })),
      h("div", { class: "form-actions" }, installBtn, checkBtn),
      fold(t("system.update.autoTitle"), t("system.update.autoNote", { n: u.every_minutes || 60, state: t(u.auto ? "system.update.autoIsOn" : "system.update.autoIsOff") }), [
        field(t("system.update.every"), every, t("system.update.everyHint")),
        h("div", { class: "field" }, auto.el, h("p", { class: "hint" }, t("system.update.autoHint"))),
      ]),
    ]);
    return null;
  }

  restartBtn.addEventListener("click", async () => {
    if (!(await confirm({ title: t("system.restartQ"), body: t("system.restartBody"), confirmLabel: t("system.restart"), danger: true }))) return;
    try { await api.post("/system/restart"); toast(t("system.restarting"), "ok"); } catch (e) { toastError(e); }
  });

  const all = () => { load(info, render); load(procs, renderProcs); load(portsCard, renderPorts); load(updateCard, () => renderUpdate(true)); };
  all();
  timer = setInterval(() => { render().catch(() => {}); renderProcs().catch(() => {}); }, 10000);
  subs.push(onEvent("middleware.", all));
  subs.push(onEvent("update.", () => renderUpdate(false).catch(() => {})));
  subs.push(onEvent("instance.", () => { renderProcs().catch(() => {}); renderPorts().catch(() => {}); }));
  return { el: p.el, destroy() { clearInterval(timer); vitals.destroy(); reqs.destroy(); reqsAll.destroy(); subs.forEach((u) => u()); } };
}
