// Access & WAF for one site: forms over the manifest's access and waf
// sections, saved with a PATCH that only touches those two keys so a
// concurrent edit to run or deploy is never overwritten by this form.

import { h, clear, copy } from "../lib/dom.js";
import { icon } from "../lib/icons.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { store } from "../lib/store.js";
import { field, toggle, confirm } from "../components/dialog.js";
import { notice } from "../components/page.js";
import { toast, toastError } from "../components/toast.js";
import { vpnSiteCard } from "./vpn.js";

const lines = (arr) => (arr || []).join("\n");
const parseLines = (s) => s.split(/[\n,]/).map((x) => x.trim()).filter(Boolean);

export function accessTab(domain) {
  const s = store.site(domain);
  const m = s.manifest || {};
  const access = m.access || {};
  const waf = m.waf || {};
  const rate = waf.rate || {};

  const password = h("input", { type: "password", autocomplete: "new-password", value: access.password || "" });
  const allow = h("textarea", { class: "mono", value: lines(access.allow), rows: "3" });
  const denyIPs = h("textarea", { class: "mono", value: lines(access.deny), rows: "3" });
  const deny = h("input", { type: "text", class: "mono", value: (access.deny_countries || []).join(", ") });
  // Zones: a path with its own password, message and addresses, asked on
  // top of the site's (access.routes). A list to edit, not JSON in a box:
  // with JSON only, protecting /admin alone was in practice impossible and
  // the operator protected all of the site or none of it.
  const zones = zonesEditor(access.routes || {});

  const requests = h("input", { type: "number", min: "0", value: rate.requests ?? "" });
  const window_ = h("input", { type: "text", class: "mono", placeholder: "1m", value: rate.window || "" });
  const burst = h("input", { type: "number", min: "0", value: rate.burst ?? "" });
  const maxBody = h("input", { type: "text", class: "mono", placeholder: "20m", value: waf.max_body || "" });
  // Off keeps the limits typed below, so switching it back on restores
  // them; they just do not apply while it is off.
  const off = toggle(t("access.wafOff"), { checked: !!waf.off });
  const limits = [requests, window_, burst, maxBody];
  const paintOff = () => limits.forEach((x) => { x.disabled = off.input.checked; });
  off.input.addEventListener("change", paintOff);
  paintOff();

  const save = h("button", { type: "submit", class: "btn primary" }, t("common.save"));
  const err = h("div", { class: "error-box", hidden: true, role: "alert" });
  const form = h("form", { novalidate: true },
    h("div", { class: "card" },
      h("h2", null, t("access.title")),
      h("p", { class: "small muted" }, t("access.hint")),
      field(t("access.password"), password, t("access.passwordHint")),
      field(t("access.allow"), allow, t("access.allowHint")),
      field(t("access.deny"), denyIPs, t("access.denyIPsHint")),
      field(t("access.denyCountries"), deny, t("access.denyHint")),
      h("h3", null, t("access.zones")),
      h("p", { class: "small muted" }, t("access.zonesHint")),
      zones.el,
    ),
    h("div", { class: "card" },
      h("h2", null, t("access.wafTitle")),
      off.el,
      h("p", { class: "small muted" }, t("access.wafOffHint")),
      h("div", { class: "form-row site-limits" },
        field(t("access.rateRequests"), requests),
        field(t("access.rateWindow"), window_),
        field(t("access.rateBurst"), burst),
        field(t("access.maxBody"), maxBody, t("access.maxBodyHint")),
      ),
      h("p", { class: "small muted" }, t("access.attackMoved")),
      err,
      h("div", { class: "form-actions" }, save),
    ),
  );

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    err.hidden = true;
    let routesObj;
    try {
      routesObj = zones.value();
    } catch (ex) {
      err.textContent = ex.message;
      err.hidden = false;
      return;
    }
    const newAccess = {};
    if (password.value) newAccess.password = password.value;
    // Not on this form, so carried as it was: the PATCH replaces the
    // whole access section and would drop it.
    if (access.message) newAccess.message = access.message;
    // The key has its own card; the form carries what the store holds,
    // which that card keeps current.
    const key = ((store.site(domain).manifest || {}).access || {}).key;
    if (key) newAccess.key = key;
    const al = parseLines(allow.value); if (al.length) newAccess.allow = al;
    const di = parseLines(denyIPs.value); if (di.length) newAccess.deny = di;
    const dc = parseLines(deny.value).map((x) => x.toUpperCase()); if (dc.length) newAccess.deny_countries = dc;
    if (routesObj && Object.keys(routesObj).length) newAccess.routes = routesObj;
    const newWaf = {};
    if (off.input.checked) newWaf.off = true;
    const r = {};
    if (requests.value !== "") r.requests = Number(requests.value);
    if (window_.value) r.window = window_.value.trim();
    if (burst.value !== "") r.burst = Number(burst.value);
    if (Object.keys(r).length) newWaf.rate = r;
    if (maxBody.value) newWaf.max_body = maxBody.value.trim();
    // Saving must not switch off a manifest under attack through the old
    // boolean; the Security tab's switch moves it to `attack`.
    if (waf.under_attack && !m.attack) newWaf.under_attack = true;
    save.classList.add("busy");
    try {
      const updated = await api.patch(`/sites/${enc(domain)}/manifest`, { access: newAccess, waf: newWaf });
      if (updated && updated.domain) store.putSite(updated);
      toast(t("access.saved"), "ok");
    } catch (ex) {
      err.textContent = ex.message;
      err.hidden = false;
    } finally { save.classList.remove("busy"); }
  });

  // The VPN choice saves on its own: it writes another key of the
  // manifest, and one form must not carry the other's half-typed values.
  return { el: h("div", { class: "cards" }, keyCard(domain), form, vpnSiteCard(domain)), destroy() {} };
}

// keyCard hides the site behind a key in its address: without it once,
// its names answer like a name the house does not serve. The key is made
// by the house and shown once here; the manifest keeps its hash.
function keyCard(domain) {
  const el = h("div", { class: "card" });
  let shown = null;
  const refresh = async () => {
    try { store.putSite(await api.get(`/sites/${enc(domain)}`)); } catch { /* the next load shows it */ }
    paint();
  };
  const turnOn = async (again) => {
    if (!(await confirm({ title: again ? t("siteKey.newQ") : t("siteKey.onQ"), body: t("siteKey.onBody"), confirmLabel: again ? t("siteKey.new") : t("siteKey.on") }))) return;
    try {
      const r = await api.post(`/sites/${enc(domain)}/access/key`, {});
      shown = r.url;
      await refresh();
    } catch (e) { toastError(e); }
  };
  function paint() {
    const on = Boolean(((store.site(domain).manifest || {}).access || {}).key);
    const actions = [];
    if (on) {
      const again = h("button", { type: "button", class: "btn sm" }, icon("refresh"), t("siteKey.new"));
      again.addEventListener("click", () => turnOn(true));
      const off = h("button", { type: "button", class: "btn sm danger ghost" }, t("siteKey.off"));
      off.addEventListener("click", async () => {
        if (!(await confirm({ title: t("siteKey.offQ"), body: t("siteKey.offBody"), confirmLabel: t("siteKey.off"), danger: true }))) return;
        try { await api.del(`/sites/${enc(domain)}/access/key`); shown = null; toast(t("siteKey.offDone"), "ok"); await refresh(); } catch (e) { toastError(e); }
      });
      actions.push(again, off);
    } else {
      const btn = h("button", { type: "button", class: "btn sm primary" }, icon("lock"), t("siteKey.on"));
      btn.addEventListener("click", () => turnOn(false));
      actions.push(btn);
    }
    const body = [];
    if (on) body.push(notice("ok", t("siteKey.isOn")));
    if (shown) {
      const url = shown;
      const box = h("textarea", { class: "mono code-box", rows: "2", readonly: true }, url);
      const cp = h("button", { type: "button", class: "btn sm" }, icon("copy"), t("common.copy"));
      cp.addEventListener("click", async () => {
        if (await copy(url)) { toast(t("common.copied"), "ok"); return; }
        box.select();
        toast(t("common.copyFailed"), "bad");
      });
      body.push(h("p", { class: "small" }, t("siteKey.once")), box, h("div", { class: "row" }, cp));
    }
    clear(el).append(
      h("div", { class: "row" }, h("h2", { class: "grow" }, t("siteKey.title")), ...actions),
      h("p", { class: "small muted" }, t("siteKey.hint")),
      ...body);
  }
  paint();
  return el;
}

// zonesEditor edits access.routes as rows: path, password, message, allowed
// addresses. A password left empty keeps the one the zone has (the
// manifest holds its hash, never shown); "remove password" drops it.
function zonesEditor(routes) {
  const list = h("div", { class: "stack-sm" });
  const add = h("button", { type: "button", class: "btn sm" }, icon("plus"), t("access.zoneAdd"));
  const el = h("div", { class: "stack-sm zones" }, list, add);
  const rows = [];

  function row(path, ra) {
    ra = ra || {};
    const hadPassword = Boolean(ra.password);
    let dropPassword = false;
    const pathIn = h("input", { type: "text", class: "mono", value: path || "", placeholder: "/admin", autocomplete: "off", spellcheck: "false" });
    const pw = h("input", { type: "password", autocomplete: "new-password", placeholder: hadPassword ? t("access.zoneKeep") : "" });
    const msg = h("input", { type: "text", value: ra.message || "", placeholder: t("access.zoneMessagePh") });
    const allow = h("textarea", { class: "mono", rows: "2", value: lines(ra.allow), placeholder: "203.0.113.0/24" });
    const state = h("span", { class: "badge " + (hadPassword ? "ok" : "") }, t(hadPassword ? "access.zoneHasPassword" : "access.zoneNoPassword"));
    const dropBtn = h("button", { type: "button", class: "btn sm ghost", hidden: !hadPassword }, t("access.zoneDropPassword"));
    dropBtn.addEventListener("click", () => {
      dropPassword = true;
      state.className = "badge warn";
      state.textContent = t("access.zoneWillDrop");
      dropBtn.hidden = true;
    });
    const remove = h("button", { type: "button", class: "btn sm ghost", "aria-label": t("access.zoneRemove") }, icon("trash"), t("access.zoneRemove"));
    const box = h("div", { class: "zone" },
      h("div", { class: "zone-head" }, field(t("access.zonePath"), pathIn), h("div", { class: "row" }, state, dropBtn, remove)),
      h("div", { class: "form-row" }, field(t("access.zonePassword"), pw), field(t("access.zoneMessage"), msg)),
      field(t("access.zoneAllow"), allow, t("access.zoneAllowHint")));
    const r = {
      box,
      value() {
        const p = pathIn.value.trim();
        if (!p) return null;
        if (!p.startsWith("/")) throw new Error(t("access.zoneBadPath", { path: p }));
        const out = { ...ra };
        delete out.password;
        if (pw.value) out.password = pw.value;
        else if (hadPassword && !dropPassword) out.password = ra.password;
        if (msg.value.trim()) out.message = msg.value.trim(); else delete out.message;
        const al = parseLines(allow.value);
        if (al.length) out.allow = al; else delete out.allow;
        if (!out.password && !(out.allow && out.allow.length) && !(out.deny && out.deny.length)) {
          throw new Error(t("access.zoneEmpty", { path: p }));
        }
        return [p, out];
      },
    };
    remove.addEventListener("click", () => {
      rows.splice(rows.indexOf(r), 1);
      box.remove();
      paintEmpty();
    });
    rows.push(r);
    list.append(box);
    return r;
  }
  const empty = h("p", { class: "site-none" }, t("access.zonesNone"));
  function paintEmpty() { empty.hidden = rows.length > 0; }
  list.append(empty);
  for (const [p, ra] of Object.entries(routes)) row(p, ra);
  paintEmpty();
  add.addEventListener("click", () => { row("", {}).box.querySelector("input").focus(); paintEmpty(); });
  return {
    el,
    value() {
      const out = {};
      for (const r of rows) {
        const v = r.value();
        if (!v) continue;
        if (out[v[0]]) throw new Error(t("access.zoneTwice", { path: v[0] }));
        out[v[0]] = v[1];
      }
      return Object.keys(out).length ? out : undefined;
    },
  };
}
