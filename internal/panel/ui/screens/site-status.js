// The site's public status page, on its page: the switch, where it
// answers, and the uptime the house has counted, which exists whether or
// not the page is on (internal/status).

import { h, clear } from "../lib/dom.js";
import { t, currentLang } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { toggle } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";

export function statusCard(domain, m) {
  const page = m.status_page || null;
  const where = (page && page.path) || "/status";
  const sw = toggle(t("status.on"), { checked: Boolean(page) });
  const info = h("div");
  sw.input.addEventListener("change", async () => {
    sw.input.disabled = true;
    try {
      await api.patch(`/sites/${enc(domain)}/manifest`, { status_page: sw.input.checked ? (page || {}) : null });
      toast(sw.input.checked ? t("status.turnedOn", { path: where }) : t("status.turnedOff"), "ok");
    } catch (e) { sw.input.checked = !sw.input.checked; toastError(e); }
    sw.input.disabled = false;
  });
  const el = h("section", { class: "card site-section" },
    h("div", { class: "log-bar" }, h("h2", { class: "grow" }, t("status.title")), sw.el),
    h("p", { class: "small muted" }, t("status.hint")),
    info);
  const pct = (f) => (f < 0 ? "—" : (f >= 1 ? "100" : Math.min(99.99, Math.floor(f * 10000) / 100).toLocaleString(currentLang())) + "%");
  api.get(`/sites/${enc(domain)}/uptime`).then((rep) => {
    clear(info);
    if (page) {
      const href = `https://${domain}${where}`;
      info.append(h("p", null, h("a", { href, target: "_blank", rel: "noopener" }, href)));
    }
    const u = rep.uptime || {};
    info.append(h("p", { class: "small" }, t("status.uptime", { d1: pct(u["1"] ?? -1), d30: pct(u["30"] ?? -1), d90: pct(u["90"] ?? -1) })));
    const last = (rep.incidents || [])[0];
    if (last) {
      info.append(h("p", { class: "small muted" }, last.to
        ? t("status.lastOutage", { when: new Date(last.from).toLocaleString(currentLang()), reason: last.reason || "—" })
        : t("status.outageNow", { when: new Date(last.from).toLocaleString(currentLang()), reason: last.reason || "—" })));
    }
  }).catch(() => {});
  return el;
}
