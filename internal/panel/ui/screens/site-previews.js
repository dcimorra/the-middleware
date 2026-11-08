// Previews on a production site's page: the switch and which branches
// get one (manifest previews), and the previews there are, each a site of
// its own with its branch and its last push. The work is the house's
// (internal/sites, previews.go), on GitHub's pushes.

import { h, clear, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { toggle, confirm } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";

export function previewsCard(domain, m) {
  const cfg = m.previews || null;
  const sw = toggle(t("previews.on"), { checked: Boolean(cfg) });
  const globs = h("input", { type: "text", value: ((cfg && cfg.branches) || []).join(", "), placeholder: t("previews.allBranches"), "aria-label": t("previews.branches"), spellcheck: "false", autocomplete: "off" });
  const settings = h("div", { class: "field" }, h("label", null, t("previews.branches")), globs);
  settings.hidden = !cfg;
  const list = h("div");

  async function save() {
    const branches = globs.value.split(",").map((s) => s.trim()).filter(Boolean);
    const body = sw.input.checked ? { previews: { ...(cfg || {}), branches: branches.length ? branches : undefined } } : { previews: null };
    try {
      await api.patch(`/sites/${enc(domain)}/manifest`, body);
      toast(sw.input.checked ? t("previews.saved") : t("previews.off"), "ok");
    } catch (e) { toastError(e); }
  }
  sw.input.addEventListener("change", () => { settings.hidden = !sw.input.checked; save(); });
  globs.addEventListener("change", save);

  async function load() {
    let r;
    try { r = await api.get(`/sites/${enc(domain)}/previews`); } catch { return; }
    clear(list);
    const items = r.previews || [];
    if (!items.length) {
      if (cfg) list.append(h("p", { class: "small muted" }, t("previews.none")));
      return;
    }
    list.append(h("ul", { class: "site-addr-list" }, ...items.map((p) => {
      const del = h("button", { type: "button", class: "btn small" }, t("previews.remove"));
      del.addEventListener("click", async () => {
        if (!(await confirm({ title: t("previews.removeQ", { name: p.name }), body: t("previews.removeBody"), confirmLabel: t("previews.remove"), danger: true }))) return;
        try { await api.del(`/sites/${enc(p.name)}`); load(); } catch (e) { toastError(e); }
      });
      return h("li", { class: "site-addr" },
        h("a", { href: `#/sites/${encodeURIComponent(p.name)}` }, h("code", null, p.branch)),
        h("a", { href: `https://${p.name}`, target: "_blank", rel: "noopener", class: "small" }, p.name),
        h("span", { class: "small muted" }, t("previews.pushed", { when: fmtAgo(p.pushed, t) })),
        del);
    })));
  }

  load();
  return h("section", { class: "card site-section" },
    h("div", { class: "log-bar" }, h("h2", { class: "grow" }, t("previews.title")), sw.el),
    h("p", { class: "small muted" }, t("previews.hint")), settings, list);
}
