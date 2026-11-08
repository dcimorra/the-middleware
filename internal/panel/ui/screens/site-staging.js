// The site's test copies, on its page: on production, its copies and a
// button to make one (its files and databases as they are now, behind a
// password shown once) and, after a promotion, the undo; on a copy, what
// it is a copy of and the two ways back: promote it, or copy production
// into it again. The work is the server's (internal/sites, staging.go).

import { h, clear, append, fmtTime } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { store } from "../lib/store.js";
import { navigate } from "../lib/router.js";
import { notice } from "../components/page.js";
import { open, confirm, field } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { icon } from "../lib/icons.js";

const siteLink = (d) => h("a", { href: `#/sites/${encodeURIComponent(d)}` }, d);

export function stagingCard(domain) {
  const el = h("section", { class: "card site-section" });
  const path = `/sites/${enc(domain)}/staging`;

  const notes = (list) => (list || []).length ? h("ul", { class: "small" }, ...list.map((n) => h("li", null, n))) : null;

  async function run(btn, fn) {
    if (btn.classList.contains("busy")) return;
    btn.classList.add("busy");
    btn.disabled = true;
    try { await fn(); } catch (e) { toastError(e); } finally { btn.classList.remove("busy"); btn.disabled = false; }
  }

  function create(btn) {
    const name = h("input", { type: "text", placeholder: "staging." + domain, autocomplete: "off", autocapitalize: "off", spellcheck: "false" });
    open({
      title: t("staging.createQ", { name: domain }),
      body: [h("p", null, t("staging.createBody")), field(t("staging.name"), name, t("staging.nameHint", { name: "staging." + domain }))],
      buttons: [
        { label: t("common.cancel"), value: false },
        { label: t("staging.create"), value: true, primary: true, submit: true },
      ],
    }).then((ok) => {
      if (!ok) return;
      run(btn, async () => {
        const body = {};
        const n = name.value.trim().toLowerCase();
        if (n) body.name = n;
        const r = await api.post(path, body);
        if (r.site && r.site.domain) store.putSite(r.site);
        const copy = (r.site && r.site.domain) || n || "staging." + domain;
        await open({
          title: t("staging.created", { name: copy }),
          body: [
            r.password ? h("p", null, t("staging.passwordOnce")) : null,
            r.password ? h("p", null, h("code", { class: "staging-password" }, r.password)) : null,
            (r.notes || []).length ? h("p", { class: "small muted" }, t("staging.notes")) : null,
            notes(r.notes),
          ],
          buttons: [{ label: t("staging.openCopy"), value: true, primary: true }],
        });
        navigate(`/sites/${encodeURIComponent(copy)}`);
      });
    });
  }

  function promote(btn, d) {
    confirm({ title: t("staging.promoteQ", { name: domain, of: d.of }), body: t("staging.promoteBody", { keep: (d.keep || []).join(" ") }), confirmLabel: t("staging.promote"), danger: true }).then((ok) => {
      if (!ok) return;
      run(btn, async () => {
        await api.post(path + "/promote", {});
        toast(t("staging.promoted", { of: d.of }), "ok");
        load();
      });
    });
  }

  function refresh(btn, d) {
    confirm({ title: t("staging.refreshQ", { name: domain }), body: t("staging.refreshBody", { of: d.of }), confirmLabel: t("staging.refresh"), danger: true }).then((ok) => {
      if (!ok) return;
      run(btn, async () => {
        const r = await api.post(path + "/refresh", {});
        toast(t("staging.refreshed"), "ok");
        if ((r.notes || []).length) toast(r.notes.join(" · "), "warn");
        load();
      });
    });
  }

  function undo(btn) {
    confirm({ title: t("staging.undoQ", { name: domain }), body: t("staging.undoBody"), confirmLabel: t("staging.undo"), danger: true }).then((ok) => {
      if (!ok) return;
      run(btn, async () => {
        await api.post(path + "/undo", {});
        toast(t("staging.undone"), "ok");
        load();
      });
    });
  }

  function draw(d) {
    const head = [h("h2", null, t("staging.title"))];
    if (d.of) {
      const pr = h("button", { type: "button", class: "btn primary" }, icon("upload"), t("staging.promote"));
      pr.addEventListener("click", () => promote(pr, d));
      const rf = h("button", { type: "button", class: "btn" }, icon("refresh"), t("staging.refresh"));
      rf.addEventListener("click", () => refresh(rf, d));
      append(clear(el), [...head,
        h("p", null, t("staging.isCopy"), " ", siteLink(d.of)),
        h("p", { class: "small muted" }, t("staging.keepHint", { keep: (d.keep || []).join(" ") })),
        h("div", { class: "row-actions" }, pr, rf)]);
      return;
    }
    const mk = h("button", { type: "button", class: "btn" }, icon("plus"), t("staging.create"));
    mk.addEventListener("click", () => create(mk));
    let undoRow = null;
    if (d.before) {
      const ub = h("button", { type: "button", class: "btn danger" }, t("staging.undo"));
      ub.addEventListener("click", () => undo(ub));
      undoRow = notice("info", t("staging.undoable", { when: fmtTime(d.before) }), " ", ub);
    }
    const copies = (d.copies || []).length
      ? h("ul", { class: "site-addr-list" }, ...d.copies.map((c) => h("li", { class: "site-addr" }, icon("copy", { size: 16 }), h("span", { class: "grow" }, siteLink(c)))))
      : h("p", { class: "small muted" }, t("staging.none"));
    append(clear(el), [...head, h("p", { class: "small muted" }, t("staging.hint")), copies, undoRow, h("div", { class: "row-actions" }, mk)]);
  }

  async function load() {
    try { draw(await api.get(path)); } catch (e) { append(clear(el), [h("h2", null, t("staging.title")), notice("bad", e.message || String(e))]); }
  }
  load();
  return el;
}
