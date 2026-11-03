// A site's database copies: each backup its manifest describes (what is
// copied, when, how many are kept), its last result, and its copies to
// download, restore or delete. The form writes the manifest's backups
// the way the agent does, so what the panel sets up and what the agent
// sets up are one thing, read back the same.

import { icon } from "../lib/icons.js";
import { h, clear, fmtTime, fmtAgo, fmtBytes } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc, apiURL } from "../lib/api.js";
import { onEvent } from "../lib/stream.js";
import { can } from "../lib/access.js";
import { store } from "../lib/store.js";
import { load, empty, errorBox, table, cell } from "../components/page.js";
import { open, confirm, field, checkbox } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { cronWords } from "./tasks.js";

const ENGINES = ["mysql", "postgres", "sqlite", "mongodb", "command"];

// The schedules people ask for, in words; anything else is written as
// cron in the field below.
const PRESETS = [
  { key: "daily", cron: "30 3 * * *" },
  { key: "twice", cron: "30 3,15 * * *" },
  { key: "every6h", cron: "0 */6 * * *" },
  { key: "hourly", cron: "0 * * * *" },
  { key: "weekly", cron: "30 3 * * 0" },
  { key: "manual", cron: "" },
];

export function backupsTab(domain) {
  const base = `/sites/${enc(domain)}/backups`;
  const canEdit = can(domain, "code");
  const canRun = can(domain, "operate");
  const body = h("div", { class: "cards" });
  const newBtn = canEdit ? h("button", { type: "button", class: "btn primary" }, icon("plus"), t("backups.new")) : null;
  if (newBtn) newBtn.addEventListener("click", () => edit(null));
  const el = h("div", { class: "cards" },
    h("div", { class: "site-tasks-head" },
      h("div", null, h("h2", null, t("site.tab.backups")), h("p", { class: "small muted" }, t("backups.sub"))), newBtn),
    body);
  const subs = [];

  async function render() {
    const list = await api.get(base);
    if (!list || list.length === 0) {
      return h("div", { class: "card" }, empty(t("backups.emptyTitle"), t("backups.emptyHint"), newBtn ? h("button", { type: "button", class: "btn primary", onClick: () => edit(null) }, icon("plus"), t("backups.new")) : null, "history"));
    }
    return h("div", { class: "cards" }, ...list.map(card));
  }

  function card(b) {
    const c = b.config;
    const words = c && c.schedule ? (cronWords(c.schedule) || c.schedule) : null;
    const where = c ? (c.engine === "sqlite" ? c.file : c.service ? t("backups.inService", { name: c.service }) : t("backups.inSite")) : "";
    const state = !c ? h("span", { class: "badge" }, t("backups.orphan"))
      : c.disabled ? h("span", { class: "badge" }, t("tasks.off"))
      : h("span", { class: "badge ok" }, t("tasks.on"));
    const head = h("div", { class: "backup-head" },
      h("div", { class: "backup-title" },
        h("strong", null, b.name), state,
        c ? h("span", { class: "small muted" }, [t("backups.engine." + c.engine), where, c.database ? t("backups.databaseNamed", { name: c.database === "*" ? t("backups.allDatabases") : c.database }) : null].filter(Boolean).join(" · ")) : null),
      h("div", { class: "backup-actions" },
        c && canRun ? runBtn(b) : null,
        c && canEdit ? h("button", { type: "button", class: "btn sm ghost", onClick: () => edit(b) }, icon("edit"), t("backups.edit")) : null,
        logBtn(b),
        c && canEdit ? h("button", { type: "button", class: "icon-btn sm", title: t("backups.remove"), "aria-label": t("backups.remove"), onClick: () => removeConfig(b) }, icon("trash")) : null));
    const facts = [];
    if (c) {
      facts.push(h("span", null, icon("clock", { size: 14 }), " ", words ? (c.timezone ? `${words} (${c.timezone})` : words) : t("backups.manualOnly")));
      if (b.next) facts.push(h("span", { title: fmtTime(b.next) }, t("tasks.nextIn", { when: fmtAgo(b.next, t) })));
      facts.push(h("span", null, t("backups.keeps", { n: c.keep || 7 })));
    }
    facts.push(h("span", null, t("backups.total", { n: b.copies.length, size: fmtBytes(b.bytes || 0) })));
    const lines = [h("div", { class: "backup-facts small muted" }, ...facts)];
    if (b.error) lines.push(h("p", { class: "small muted" }, t("backups.orphanHint")));
    if (b.running) lines.push(h("p", { class: "status" }, h("span", { class: "dot warn" }), t(b.running === "restore" ? "backups.restoringNow" : "backups.copyingNow")));
    if (b.last) lines.push(result(b.last, "last"));
    if (b.last_restore) lines.push(result(b.last_restore, "restore"));
    lines.push(b.copies.length ? copiesTable(b) : h("p", { class: "small muted" }, t("backups.noCopies")));
    return h("div", { class: "card backup-card" }, head, ...lines);
  }

  function result(r, what) {
    const when = h("span", { title: fmtTime(r.at) }, fmtAgo(r.at, t));
    if (r.ok) {
      return h("p", { class: "status small" }, h("span", { class: "dot ok" }),
        t(what === "last" ? "backups.lastOk" : "backups.restoreOk", { copy: r.copy || "" }), " · ", when,
        r.bytes ? " · " + fmtBytes(r.bytes) : null);
    }
    return h("div", { class: "error-box small" },
      h("strong", null, t(what === "last" ? "backups.lastFailed" : "backups.restoreFailed")), " · ", when,
      h("div", { class: "mono" }, r.error || ""));
  }

  function copiesTable(b) {
    const cols = [{ label: t("backups.col.taken") }, { label: t("backups.col.kind") }, { label: t("backups.col.size") }, { label: "", class: "right" }];
    const rows = b.copies.map((cp) => h("tr", null,
      cell(h("span", { title: cp.id }, fmtTime(cp.at)), t("backups.col.taken")),
      cell(t("backups.kind." + cp.kind), t("backups.col.kind")),
      cell(fmtBytes(cp.bytes), t("backups.col.size")),
      cell(h("div", { class: "row-actions" },
        canEdit ? h("a", { class: "btn sm ghost", href: apiURL(`${base}/${enc(b.name)}/copies/${enc(cp.id)}`), download: `${domain}-${b.name}-${cp.id}` }, icon("download"), t("backups.download")) : null,
        canEdit && b.config ? h("button", { type: "button", class: "btn sm", disabled: Boolean(b.running) || (b.config.engine === "command" && !(b.config.restore || []).length), title: b.config.engine === "command" && !(b.config.restore || []).length ? t("backups.noRestore") : "", onClick: () => restore(b, cp) }, icon("history"), t("backups.restore")) : null,
        canEdit ? h("button", { type: "button", class: "icon-btn sm", title: t("common.delete"), "aria-label": t("backups.deleteCopy"), onClick: () => delCopy(b, cp) }, icon("trash")) : null), "", "right")));
    return table(cols, rows, { responsive: true }).el;
  }

  function runBtn(b) {
    const btn = h("button", { type: "button", class: "btn sm", disabled: Boolean(b.running) }, icon("play"), t("backups.run"));
    btn.addEventListener("click", async () => {
      btn.classList.add("busy");
      try { await api.post(`${base}/${enc(b.name)}/run`); toast(t("backups.started", { name: b.name }), "ok"); refresh(); }
      catch (e) { toastError(e); } finally { btn.classList.remove("busy"); }
    });
    return btn;
  }

  function logBtn(b) {
    const btn = h("button", { type: "button", class: "btn sm ghost" }, icon("list"), t("tasks.log"));
    btn.addEventListener("click", async () => {
      const pre = h("pre", { class: "log", tabindex: "0" });
      const box = h("div", null);
      const dlg = open({ title: t("backups.logOf", { name: b.name }), body: box, wide: true, buttons: [{ label: t("common.close"), value: null }] });
      try {
        const text = await api.text(`${base}/${enc(b.name)}/log`);
        pre.textContent = text || t("tasks.logEmpty");
        clear(box).append(pre);
        pre.scrollTop = pre.scrollHeight;
      } catch (e) { clear(box).append(errorBox(e)); }
      await dlg;
    });
    return btn;
  }

  async function restore(b, cp) {
    const safety = checkbox(t("backups.safetyCopy"), { checked: true });
    const ok = await confirm({
      title: t("backups.restoreQ", { name: b.name, when: fmtTime(cp.at) }),
      body: t(b.config.engine === "sqlite" ? "backups.restoreBodySqlite" : "backups.restoreBody"),
      confirmLabel: t("backups.restore"), danger: true,
      extra: h("div", null, safety.el, h("p", { class: "small muted" }, t("backups.safetyHint"))),
    });
    if (!ok) return;
    try {
      await api.post(`${base}/${enc(b.name)}/copies/${enc(cp.id)}/restore`, { no_safety_copy: !safety.input.checked });
      toast(t("backups.restoreStarted"), "ok");
      refresh();
    } catch (e) { toastError(e); }
  }

  async function delCopy(b, cp) {
    if (!(await confirm({ title: t("backups.deleteQ", { when: fmtTime(cp.at) }), body: t("backups.deleteBody"), confirmLabel: t("common.delete"), danger: true }))) return;
    try { await api.del(`${base}/${enc(b.name)}/copies/${enc(cp.id)}`); toast(t("backups.deleted"), "ok"); refresh(); } catch (e) { toastError(e); }
  }

  // The manifest's backups as they are now, read again before every
  // write: the agent may have written them since the tab was drawn.
  async function currentManifest() {
    const s = await api.get(`/sites/${enc(domain)}`);
    store.putSite(s);
    return s.manifest || {};
  }

  async function writeBackups(map) {
    const s = await api.patch(`/sites/${enc(domain)}/manifest`, { backups: Object.keys(map).length ? map : null });
    store.putSite(s);
  }

  async function removeConfig(b) {
    if (!(await confirm({ title: t("backups.removeQ", { name: b.name }), body: t("backups.removeBody"), confirmLabel: t("backups.remove"), danger: true }))) return;
    try {
      const m = await currentManifest();
      const map = { ...(m.backups || {}) };
      delete map[b.name];
      await writeBackups(map);
      toast(t("backups.removed", { name: b.name }), "ok");
      refresh();
    } catch (e) { toastError(e); }
  }

  async function edit(b) {
    let m;
    try { m = await currentManifest(); } catch (e) { toastError(e); return; }
    const cfg = (b && b.config) || {};
    const holders = [...Object.keys(m.services || {}), ...Object.keys(m.workers || {})].sort();
    const name = h("input", { type: "text", class: "mono", value: b ? b.name : "db", readOnly: Boolean(b), pattern: "[a-z0-9-]+", maxlength: 32 });
    const engine = h("select", null, ...ENGINES.map((e) => h("option", { value: e }, t("backups.engine." + e))));
    engine.value = cfg.engine || (holders.length ? "mysql" : "sqlite");
    const service = h("select", null,
      Object.keys(m.services || {}).length ? null : h("option", { value: "" }, t("backups.inSite")),
      ...holders.map((n) => h("option", { value: n }, n)));
    service.value = cfg.service || (Object.keys(m.services || {}).length ? holders.find((n) => /db|sql|maria|postgres|mongo/.test(n)) || holders[0] : "");
    const database = h("input", { type: "text", class: "mono", value: cfg.database || "", placeholder: t("backups.databasePh") });
    const file = h("input", { type: "text", class: "mono", value: cfg.file || "", placeholder: "data/app.db" });
    const user = h("input", { type: "text", class: "mono", value: cfg.user || "", autocomplete: "off" });
    const pwEnv = h("input", { type: "text", class: "mono", value: cfg.password_env || "", placeholder: "DB_PASSWORD", autocomplete: "off" });
    const host = h("input", { type: "text", class: "mono", value: cfg.host || "", placeholder: "127.0.0.1" });
    const port = h("input", { type: "number", min: 1, max: 65535, value: cfg.port || "" });
    const dump = h("input", { type: "text", class: "mono", value: cfg.dump ? JSON.stringify(cfg.dump) : "", placeholder: '["sh", "-c", "redis-cli --rdb - "]' });
    const rest = h("input", { type: "text", class: "mono", value: cfg.restore ? JSON.stringify(cfg.restore) : "", placeholder: '["sh", "-c", "…"]' });
    const preset = h("select", null, ...PRESETS.map((p) => h("option", { value: p.key }, t("backups.preset." + p.key))), h("option", { value: "custom" }, t("backups.preset.custom")));
    const cron = h("input", { type: "text", class: "mono", value: b ? cfg.schedule || "" : PRESETS[0].cron, placeholder: "30 3 * * *" });
    const known = PRESETS.find((p) => p.cron === cron.value);
    preset.value = known ? known.key : "custom";
    let zone = "";
    try { zone = Intl.DateTimeFormat().resolvedOptions().timeZone || ""; } catch { zone = ""; }
    const tz = h("input", { type: "text", class: "mono", value: b ? cfg.timezone || "" : zone, placeholder: "Europe/Madrid" });
    const keep = h("input", { type: "number", min: 1, max: 1000, value: cfg.keep || 7 });
    const disabled = checkbox(t("backups.disabled"), { checked: Boolean(cfg.disabled) });

    const fServer = [field(t("backups.service"), service, t("backups.serviceHint")), field(t("backups.database"), database, t("backups.databaseHint"))];
    const fAuth = h("details", { class: "backup-more" }, h("summary", null, t("backups.connection")),
      field(t("backups.user"), user, t("backups.userHint")), field(t("backups.passwordEnv"), pwEnv, t("backups.passwordEnvHint")),
      field(t("backups.host"), host, t("backups.hostHint")), field(t("backups.port"), port));
    const fFile = field(t("backups.file"), file, t("backups.fileHint"));
    const fCommand = [field(t("backups.dump"), dump, t("backups.dumpHint")), field(t("backups.restoreCmd"), rest, t("backups.restoreCmdHint"))];
    const fCron = field(t("backups.cron"), cron, t("backups.cronHint"));
    const sync = () => {
      const e = engine.value;
      fServer[0].hidden = e === "sqlite";
      fServer[1].hidden = e === "sqlite" || e === "command";
      fAuth.hidden = e === "sqlite" || e === "command";
      fFile.hidden = e !== "sqlite";
      fCommand.forEach((f) => { f.hidden = e !== "command"; });
      fCron.hidden = preset.value !== "custom";
    };
    engine.addEventListener("change", sync);
    preset.addEventListener("change", () => {
      const p = PRESETS.find((x) => x.key === preset.value);
      if (p) cron.value = p.cron;
      sync();
    });
    sync();

    const done = await open({
      title: b ? t("backups.editTitle", { name: b.name }) : t("backups.newTitle"),
      wide: true,
      body: [
        field(t("backups.name"), name, b ? null : t("backups.nameHint")),
        field(t("backups.engineLabel"), engine),
        ...fServer, fFile, ...fCommand, fAuth,
        field(t("backups.when"), preset), fCron,
        field(t("backups.timezone"), tz, t("backups.timezoneHint")),
        field(t("backups.keep"), keep, t("backups.keepHint")),
        disabled.el,
        h("p", { class: "small muted" }, t("backups.formHint")),
      ],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("common.save"), submit: true, primary: true }],
      onSubmit: async (_f, showError) => {
        const n = name.value.trim();
        if (!/^[a-z0-9][a-z0-9-]{0,31}$/.test(n)) { showError(t("backups.badName")); return false; }
        const e = engine.value;
        const out = { engine: e };
        const put = (k, v) => { if (v !== "" && v !== null && v !== undefined) out[k] = v; };
        if (e === "sqlite") {
          put("file", file.value.trim());
        } else {
          put("service", service.value);
        }
        if (e === "command") {
          for (const [k, input] of [["dump", dump], ["restore", rest]]) {
            const v = input.value.trim();
            if (!v) continue;
            let arr;
            try { arr = JSON.parse(v); } catch { arr = null; }
            if (!Array.isArray(arr) || !arr.every((x) => typeof x === "string")) { showError(t("backups.badArgv", { field: k })); return false; }
            out[k] = arr;
          }
        } else if (e !== "sqlite") {
          put("database", database.value.trim());
          put("user", user.value.trim());
          put("password_env", pwEnv.value.trim());
          put("host", host.value.trim());
          if (port.value) out.port = Number(port.value);
        }
        put("schedule", cron.value.trim());
        if (out.schedule) put("timezone", tz.value.trim());
        if (Number(keep.value) && Number(keep.value) !== 7) out.keep = Number(keep.value);
        if (disabled.input.checked) out.disabled = true;
        if (cfg.note) out.note = cfg.note;
        if (cfg.timeout) out.timeout = cfg.timeout;
        const fresh = await currentManifest();
        const map = { ...(fresh.backups || {}) };
        if (!b && map[n]) { showError(t("backups.taken", { name: n })); return false; }
        map[n] = out;
        await writeBackups(map);
        return n;
      },
    });
    if (!done) return;
    toast(t("backups.saved", { name: done }), "ok");
    refresh();
    // A backup nobody saw work does not exist: offer the first copy now.
    if (!b && canRun && await confirm({ title: t("backups.firstQ"), body: t("backups.firstBody"), confirmLabel: t("backups.run") })) {
      try { await api.post(`${base}/${enc(done)}/run`); toast(t("backups.started", { name: done }), "ok"); refresh(); } catch (e) { toastError(e); }
    }
  }

  function refresh() { load(body, render); }

  refresh();
  subs.push(onEvent("backup.", (ev) => { if ((!ev || ev.subject === domain) && !document.querySelector("dialog[open]")) refresh(); }));
  return { el, destroy() { subs.forEach((u) => u()); } };
}
