// The node's browsers for its sites (settings.browsers, GET /browsers):
// how many it keeps, with the number the machine is recommended, and
// which site holds each one now, for what and since when. The operator
// frees one a site forgot; the site is told by the event.

import { h, clear, fmtAgo, fmtBytes } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { table, cell, load } from "../components/page.js";
import { confirm, field } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { icon } from "../lib/icons.js";
import { cardHead, saveBar, fold, quietEmpty } from "./settings-kit.js";

export function browsersCard() {
  const el = h("div", { class: "card set-card" });
  const list = h("div");
  // The list is live: who holds what changes while the screen is open.
  let timer = 0;

  async function render() {
    const [s, b] = await Promise.all([api.get("/settings"), api.get("/browsers")]);
    const conf = s.browsers || {};
    // Empty is the recommended number; the placeholder says which.
    const count = h("input", { type: "number", min: "0", max: String(b.max), class: "set-num", value: conf.count ?? "", placeholder: String(b.recommended) });
    const idle = h("input", { type: "number", min: "1", max: "1440", class: "set-num", value: conf.idle_minutes || "", placeholder: String(b.idle_minutes) });
    const save = h("button", { type: "submit", class: "btn primary" }, t("common.save"));
    const err = h("div", { class: "error-box", hidden: true });
    const bar = saveBar(save);
    const recHint = t("browsers.recommendedHint", { n: b.recommended, cpus: b.cpus || "?", memory: b.memory ? fmtBytes(b.memory) : "?", each: fmtBytes(b.per_browser) });
    const form = h("form", { novalidate: true },
      h("div", { class: "form-row" },
        field(t("browsers.count"), count, conf.count == null ? t("browsers.countRecommended", { n: b.recommended }) + " " + recHint : recHint),
      ),
      fold(t("settings.advanced"), t("browsers.idleNote", { n: b.idle_minutes }), [
        field(t("browsers.idle"), idle, t("browsers.idleHint")),
      ]),
      err,
      bar.el,
    );
    bar.watch(form);
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      err.hidden = true;
      const c = count.value.trim() === "" ? null : Number(count.value);
      const i = idle.value.trim() === "" ? 0 : Number(idle.value);
      if ((c !== null && !(Number.isInteger(c) && c >= 0 && c <= b.max)) || !(Number.isInteger(i) && i >= 0 && i <= 1440)) {
        err.textContent = t("browsers.bad", { max: b.max });
        err.hidden = false;
        return;
      }
      save.classList.add("busy");
      try {
        const cur = await api.get("/settings");
        const next = { ...(cur.browsers || {}), idle_minutes: i };
        if (c === null) delete next.count; else next.count = c;
        await api.put("/settings", { ...cur, browsers: next });
        bar.saved(t("settings.inForce"));
        renderList();
      } catch (ex) { err.textContent = ex.message; err.hidden = false; }
      finally { save.classList.remove("busy"); }
    });
    // append() writes a null as the word "null": the warning is left
    // out rather than passed empty.
    const parts = [cardHead(t("browsers.title"), t("browsers.hint"))];
    if (!b.available) parts.push(h("div", { class: "error-box" }, icon("alert", { size: 18 }), h("span", { class: "grow" }, t("browsers.noBinary"))));
    parts.push(form, h("div", { class: "set-recent" }, h("h3", null, t("browsers.now")), list));
    clear(el).append(...parts);
    paint(b);
    clearInterval(timer);
    timer = setInterval(() => { if (!el.isConnected) { clearInterval(timer); return; } renderList(); }, 15000);
    return null;
  }

  async function renderList() {
    try { paint(await api.get("/browsers")); } catch (e) { /* the next tick tries again */ }
  }

  function paint(b) {
    if (!b.slots.length) {
      clear(list).append(quietEmpty("monitor", t("browsers.none"), t("browsers.noneHint")));
      return;
    }
    const rows = b.slots.map((sl) => {
      let state;
      if (sl.free) state = h("span", { class: "badge ok" }, t("browsers.free"));
      else if (sl.leaving) state = h("span", { class: "badge warn", title: t("browsers.leavingHint") }, t("browsers.leaving"));
      else state = h("span", { class: "badge accent" }, sl.connected ? t("browsers.inUse") : t("browsers.taken"));
      const free = sl.free ? null : h("button", { type: "button", class: "btn sm" }, t("browsers.release"));
      if (free) free.addEventListener("click", async () => {
        const ok = await confirm({ title: t("browsers.releaseTitle", { n: sl.slot }), body: t("browsers.releaseBody", { site: sl.site }), confirmLabel: t("browsers.release"), danger: true });
        if (!ok) return;
        free.classList.add("busy");
        try { paint(await api.del(`/browsers/${enc(String(sl.slot))}`)); toast(t("browsers.released", { n: sl.slot }), "ok"); }
        catch (e) { toastError(e); free.classList.remove("busy"); }
      });
      return h("tr", null,
        cell(String(sl.slot), t("browsers.colBrowser"), "mono"),
        cell(state, t("browsers.colState")),
        cell(sl.site ? h("a", { href: "#/sites/" + enc(sl.site) }, sl.site) : "—", t("browsers.colSite")),
        cell(sl.for || "—", t("browsers.colFor")),
        cell(sl.since ? fmtAgo(sl.since, t) : "—", t("browsers.colSince")),
        cell(sl.used ? fmtAgo(sl.used, t) : "—", t("browsers.colUsed")),
        cell(free || "", null, "right"));
    });
    const tbl = table([
      { label: t("browsers.colBrowser") }, { label: t("browsers.colState") }, { label: t("browsers.colSite") },
      { label: t("browsers.colFor") }, { label: t("browsers.colSince") }, { label: t("browsers.colUsed") }, { label: "" },
    ], rows, { responsive: true });
    clear(list).append(h("p", { class: "hint" }, t("browsers.freeOf", { free: b.free, count: b.count })), tbl.el);
  }

  load(el, render);
  return el;
}
