// A "…" menu for the sites screens: what is used once in a while folds
// under it, so the few actions that matter for a site's state are the
// ones in sight. The overview used to show ten equal buttons, and the
// one that mattered (start a stopped site, release a held one) read the
// same as "download" or "delete".
//
// A <details>, as the chat's menu is: it opens and closes with the
// keyboard and without script, and nothing inline is needed for the CSP.
// It is kept here and not in components/ because only these screens use
// it; the chat has its own, drawn for its bar.

import { h } from "../lib/dom.js";
import { icon } from "../lib/icons.js";
import { toastError } from "../components/toast.js";

// items: { label, icon, onClick } for an action, { label, icon, href,
// download } for a link, { separator: true } for a line, and danger on
// either for what destroys. Null entries are skipped, so callers can
// write `cond ? item : null` inline. A link with external set opens in
// a new tab, and hint is a muted word after the label.
//
// opts.text puts words in the button and opts.icon replaces the "…":
// the site page's Open becomes one of these for a site with several
// names, and it has to read as the Open it was.
export function menu(label, items, opts = {}) {
  const list = h("div", { class: "site-menu-list", role: "menu" });
  const el = h("details", { class: "site-menu" + (opts.class ? " " + opts.class : "") },
    h("summary", { class: "site-menu-btn", "aria-label": label, title: label, role: "button" }, opts.icon ? null : icon("more", { stroke: 2.5, size: 18 }), opts.text ? h("span", null, opts.text) : null, opts.icon ? icon(opts.icon, { size: 14 }) : null),
    list);
  const close = () => { el.open = false; };
  let lastWasSep = true;
  for (const it of items) {
    if (!it) continue;
    if (it.separator) {
      if (!lastWasSep) list.append(h("div", { class: "site-menu-sep", role: "separator" }));
      lastWasSep = true;
      continue;
    }
    lastWasSep = false;
    const cls = "site-menu-item" + (it.danger ? " danger" : "");
    if (it.href) {
      list.append(h("a", {
        class: cls, role: "menuitem", href: it.href, download: it.download || null, title: it.title || null, onClick: close,
        target: it.external ? "_blank" : null, rel: it.external ? "noopener" : null,
      }, it.icon ? icon(it.icon, { size: 16 }) : null, h("span", null, it.label), it.hint ? h("span", { class: "site-menu-hint" }, it.hint) : null));
      continue;
    }
    const b = h("button", { type: "button", class: cls, role: "menuitem", title: it.title || null }, it.icon ? icon(it.icon, { size: 16 }) : null, h("span", null, it.label));
    b.addEventListener("click", async () => {
      close();
      try { await it.onClick(); } catch (e) { toastError(e); }
    });
    list.append(b);
  }
  // A click anywhere else closes it, and Escape too, as any menu does.
  // The listener goes with the menu: a screen re-renders its menus on
  // every stream event, and each one left a listener behind otherwise.
  const outside = (e) => {
    if (!el.isConnected) { document.removeEventListener("click", outside); document.removeEventListener("keydown", onKey); return; }
    if (el.open && !el.contains(e.target)) close();
  };
  const onKey = (e) => {
    if (!el.isConnected) { document.removeEventListener("click", outside); document.removeEventListener("keydown", onKey); return; }
    if (e.key === "Escape" && el.open) { close(); el.querySelector("summary").focus(); }
  };
  el.addEventListener("toggle", () => {
    if (el.open) { document.addEventListener("click", outside); document.addEventListener("keydown", onKey); }
    else { document.removeEventListener("click", outside); document.removeEventListener("keydown", onKey); }
  });
  return el;
}
