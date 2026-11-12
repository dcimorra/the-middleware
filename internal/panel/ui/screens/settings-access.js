// Who may reach the panel: passkeys, the address lists and the closed
// password. Two cards of the Settings screen, apart from it because they
// are the ones that can lock the operator out, and each guards that.

import { h, clear, fmtTime, fmtAgo, copy } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { table, cell, notice, load } from "../components/page.js";
import { open, confirm, field, toggle } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { icon } from "../lib/icons.js";
import * as passkeys from "../lib/passkeys.js";
import { cardHead, saveBar, fold, quietEmpty } from "./settings-kit.js";

export function passkeysCard() {
  const el = h("div", { class: "card pad0 set-card" });
  async function render() {
    const list = await api.get("/auth/passkeys");
    const add = h("button", { type: "button", class: "btn sm primary" }, icon("key"), t("passkeys.add"));
    add.disabled = !passkeys.supported();
    add.addEventListener("click", async () => {
      const name = h("input", { type: "text", placeholder: t("passkeys.namePlaceholder"), required: true });
      const made = await open({
        title: t("passkeys.add"),
        body: [h("p", { class: "dim small" }, t("passkeys.addBody", { host: location.hostname })), field(t("passkeys.name"), name)],
        buttons: [{ label: t("common.cancel"), value: null }, { label: t("passkeys.create"), submit: true, primary: true }],
        onSubmit: async (_f, showError) => {
          if (!name.value.trim()) { showError(t("passkeys.nameRequired")); return false; }
          try { return await passkeys.register(name.value.trim()); }
          catch (e) { showError(e.name === "NotAllowedError" ? t("passkeys.cancelled") : e.message); return false; }
        },
      });
      if (made) { toast(t("passkeys.added"), "ok"); load(el, render); }
    });
    const rows = list.map((k) => {
      const del = h("button", { type: "button", class: "btn sm danger ghost" }, t("passkeys.remove"));
      del.addEventListener("click", async () => {
        if (!(await confirm({ title: t("passkeys.removeQ", { name: k.name }), body: t("passkeys.removeBody"), confirmLabel: t("passkeys.remove"), danger: true }))) return;
        try { await api.del(`/auth/passkeys/${enc(k.id)}`); toast(t("passkeys.removed"), "ok"); load(el, render); } catch (e) { toastError(e); }
      });
      return h("tr", null,
        cell(k.name, null, "primary"),
        cell(h("code", null, k.rp_id), t("passkeys.boundTo")),
        cell(h("span", { title: fmtTime(k.created) }, fmtAgo(k.created, t)), t("settings.created")),
        cell(k.last_used ? h("span", { title: fmtTime(k.last_used) }, fmtAgo(k.last_used, t)) : h("span", { class: "muted" }, t("settings.neverUsed")), t("settings.lastUsed")),
        cell(del, null, "right"));
    });
    clear(el).append(
      h("div", { class: "card-pad" }, cardHead(t("passkeys.title"), passkeys.supported() ? t("passkeys.hint") : t("passkeys.unsupported"), add)),
      rows.length ? table([{ label: t("passkeys.name") }, { label: t("passkeys.boundTo") }, { label: t("settings.created") }, { label: t("settings.lastUsed") }, { label: "" }], rows, { responsive: true }).el
        : quietEmpty("key", t("passkeys.none"), t("passkeys.noneHint")),
    );
    return null;
  }
  load(el, render);
  return el;
}

// An IPv6 address rotates by itself every few days; its /64 does not.
const suggest = (ip) => (ip && ip.includes(":") ? ip.split(":").slice(0, 4).join(":") + "::/64" : ip);

export function panelAccessCard() {
  const el = h("div", { class: "card set-card" });
  // A save draws the card again with the lists as the server keeps them;
  // what the save said survives that, or it would flash and vanish.
  let savedNow = false;
  async function render() {
    const a = await api.get("/auth/panel-access");
    const r = a.rules || {};
    const allow = h("textarea", { class: "mono", rows: "3", value: (r.allow || []).join("\n") });
    const deny = h("textarea", { class: "mono", rows: "2", value: (r.deny || []).join("\n") });
    const only = toggle(t("panelAccess.passkeyOnly"), { checked: Boolean(r.passkey_only) });
    const err = h("div", { class: "error-box", hidden: true, role: "alert" });
    const save = h("button", { type: "button", class: "btn primary" }, t("common.save"));
    const bar = saveBar(save);
    if (savedNow) { bar.saved(); savedNow = false; }
    const mine = suggest(a.your_address);
    const addMine = h("button", { type: "button", class: "btn sm" }, t("panelAccess.addMine", { ip: mine || "" }));
    addMine.hidden = !mine || a.from_machine;
    addMine.addEventListener("click", () => { allow.value = (allow.value.trim() ? allow.value.trim() + "\n" : "") + mine; bar.dirty(); });
    save.addEventListener("click", async () => {
      err.hidden = true;
      const lines = (s) => s.split(/[\n,]/).map((x) => x.trim()).filter(Boolean);
      save.classList.add("busy");
      try {
        await api.put("/auth/panel-access", { allow: lines(allow.value), deny: lines(deny.value), passkey_only: only.input.checked });
        savedNow = true;
        load(el, render);
      } catch (e) { err.textContent = e.message; err.hidden = false; }
      finally { save.classList.remove("busy"); }
    });
    const form = h("div", null, ...[
      a.open_until ? notice("bad", t("panelAccess.openNow", { at: fmtTime(a.open_until) })) : null,
      h("div", { class: "set-you" }, h("span", null, a.from_machine ? t("panelAccess.fromMachine") : t("panelAccess.yours", { ip: a.your_address || "?" })), addMine),
      field(t("panelAccess.allow"), allow, t("panelAccess.allowHint")),
      // The deny list is the rarer one: folded, unless it holds something.
      fold(t("panelAccess.deny"), (r.deny || []).length ? t("panelAccess.denyCount", { n: r.deny.length }) : t("panelAccess.denyNone"), [field(t("panelAccess.deny"), deny)], (r.deny || []).length > 0),
      h("div", { class: "field set-switch-field" }, only.el, h("div", { class: "hint" }, t("panelAccess.passkeyOnlyHint"))),
      err,
      bar.el,
    ].filter(Boolean)); // a native append writes null as the text "null"
    bar.watch(form);
    clear(el).append(cardHead(t("panelAccess.title"), t("panelAccess.hint")), form);
    return null;
  }
  load(el, render);
  return el;
}

// The panel's key: without it in the address once, the panel's name
// answers "no site here". The key is shown once, when it is made; the
// browser that turns it on gets the cookie with the answer, so it is not
// shut out by its own switch. The way back if it is lost is the shell's.
// Always this node's ({ node: "" }): the address shown is this page's,
// and another node's key in it would open nothing.
export function panelKeyCard() {
  const el = h("div", { class: "card set-card" });
  // The key of the last switch, kept only until the card is drawn again:
  // the server keeps its hash alone.
  let shown = null;
  async function render() {
    const k = await api.get("/inbound/panel-key", { node: "" });
    const turnOn = async (again) => {
      const q = again ? t("panelKey.newQ") : t("panelKey.onQ");
      if (!(await confirm({ title: q, body: t("panelKey.onBody"), confirmLabel: again ? t("panelKey.new") : t("panelKey.on") }))) return;
      try {
        const r = await api.post("/inbound/panel-key", {}, { node: "" });
        shown = `${location.origin}/?${r.param}=${r.key}`;
        load(el, render);
      } catch (e) { toastError(e); }
    };
    const actions = [];
    if (k.on) {
      const again = h("button", { type: "button", class: "btn sm" }, icon("refresh"), t("panelKey.new"));
      again.addEventListener("click", () => turnOn(true));
      const off = h("button", { type: "button", class: "btn sm danger ghost" }, t("panelKey.off"));
      off.addEventListener("click", async () => {
        if (!(await confirm({ title: t("panelKey.offQ"), body: t("panelKey.offBody"), confirmLabel: t("panelKey.off"), danger: true }))) return;
        try { await api.del("/inbound/panel-key", { node: "" }); shown = null; toast(t("panelKey.offDone"), "ok"); load(el, render); } catch (e) { toastError(e); }
      });
      actions.push(again, off);
    } else {
      const on = h("button", { type: "button", class: "btn sm primary" }, icon("lock"), t("panelKey.on"));
      on.addEventListener("click", () => turnOn(false));
      actions.push(on);
    }
    const body = [];
    if (k.on) body.push(notice("ok", t("panelKey.isOn", { at: fmtTime(k.since) })));
    if (shown) {
      const box = h("textarea", { class: "mono code-box", rows: "2", readonly: true }, shown);
      const cp = h("button", { type: "button", class: "btn sm" }, icon("copy"), t("common.copy"));
      const url = shown;
      cp.addEventListener("click", async () => {
        if (await copy(url)) { toast(t("common.copied"), "ok"); return; }
        box.select();
        toast(t("common.copyFailed"), "bad");
      });
      body.push(h("p", { class: "small" }, t("panelKey.once")), box, h("div", { class: "row" }, cp));
    }
    body.push(h("p", { class: "hint" }, t("panelKey.cli")));
    clear(el).append(cardHead(t("panelKey.title"), t("panelKey.hint"), ...actions), ...body);
    return null;
  }
  load(el, render);
  return el;
}

// The red strip every screen carries while the panel is opened from the
// shell: an open door that nobody sees is a door left open.
export function openBanner(until, onClosed) {
  const close = h("button", { type: "button", class: "btn sm" }, t("panelAccess.closeNow"));
  const bar = h("div", { class: "open-banner", role: "alert" }, icon("alert"), h("span", { class: "grow" }, t("panelAccess.banner", { at: fmtTime(until) })), close);
  close.addEventListener("click", async () => {
    try { await api.del("/auth/panel-open"); bar.remove(); onClosed && onClosed(); toast(t("panelAccess.closed"), "ok"); } catch (e) { toastError(e); }
  });
  return bar;
}
