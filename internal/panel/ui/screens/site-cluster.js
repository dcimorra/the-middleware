// The site's cluster, on its page: copies on other nodes of the network
// that this node hands requests to when it is saturated or its own
// instance is down. On or off, how many copies, from what load it shares,
// the databases the copies reach here (the gateway), and each copy's
// state. Everything is written into the manifest (cluster); the server
// does the rest in its rounds.

import { h, clear } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { notice, table, cell } from "../components/page.js";
import { confirm, field, toggle } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";

const pct = (v) => `${Math.round((v || 0) * 100)} %`;

export function clusterCard(domain) {
  const el = h("section", { class: "card site-section" });
  const path = `/sites/${enc(domain)}/cluster`;
  let timer = null;
  // The form is drawn once per cluster switch: the rounds redraw only the
  // table, so what is being typed stays.
  let formFor = null;
  const form = h("div");
  const rows = h("div");

  const patch = async (cluster) => {
    try {
      await api.patch(`/sites/${enc(domain)}/manifest`, { cluster });
      toast(t("cluster.saved"), "ok");
      formFor = null;
      load();
    } catch (e) { toastError(e); }
  };

  function drawForm(d) {
    const c = d.cluster || {};
    const replicas = h("input", { type: "number", min: "1", max: "16", value: String(d.want || 1) });
    const spill = h("input", { type: "number", min: "10", max: "400", value: String(Math.round((d.spill_at || 0.7) * 100)) });
    const mode = h("select", null,
      h("option", { value: "overflow" }, t("cluster.modeOverflow")),
      h("option", { value: "spread" }, t("cluster.modeSpread")));
    mode.value = c.mode || "overflow";
    const gateway = h("input", { type: "text", placeholder: "127.0.0.1:3306", value: (d.gateway || []).join(", ") });
    const save = h("button", { class: "btn primary", type: "button" }, t("common.save"));
    save.addEventListener("click", () => {
      const next = { ...c, replicas: Number(replicas.value) || 1, mode: mode.value === "overflow" ? undefined : mode.value };
      const sp = Number(spill.value);
      next.spill = sp && sp !== 70 ? sp : undefined;
      const gw = gateway.value.split(",").map((x) => x.trim()).filter(Boolean);
      next.gateway = gw.length ? gw : undefined;
      patch(next);
    });
    clear(form).append(
      field(t("cluster.replicas"), replicas, t("cluster.replicasHint")),
      field(t("cluster.mode"), mode, t("cluster.modeHint")),
      field(t("cluster.spill"), spill, t("cluster.spillHint")),
      field(t("cluster.gateway"), gateway, t("cluster.gatewayHint")),
      h("div", { class: "row" }, save));
  }

  function drawRows(d) {
    const head = h("p", { class: "small muted" },
      t("cluster.here", { load: pct(d.load && d.load.per_core), cores: (d.load && d.load.cores) || "?", spill: pct(d.spill_at) }),
      " ", d.sharing ? h("strong", null, t("cluster.sharing")) : t("cluster.notSharing"));
    const list = d.replicas || [];
    const missing = Math.max(0, (d.want || 0) - list.length);
    clear(rows).append(head,
      list.length ? table([
        { label: t("cluster.col.node") }, { label: t("cluster.col.state") }, { label: t("cluster.col.load") }, { label: t("cluster.col.detail") },
      ], list.map((r) => h("tr", null,
        cell(r.node || r.node_id, t("cluster.col.node")),
        cell(h("span", { class: "status" }, h("span", { class: "dot " + (r.state === "ready" && r.alive ? "ok" : r.state === "failed" || r.state === "unreachable" ? "bad" : "warn") }), t("cluster.state." + r.state)), t("cluster.col.state")),
        cell(r.load ? `${pct(r.load.per_core)} · ${r.load.cores}` : "—", t("cluster.col.load")),
        cell(r.refused || r.error || "—", t("cluster.col.detail")),
      )), { responsive: true }).el : null,
      missing ? notice("warn", t("cluster.missing", { n: missing })) : null);
  }

  async function load() {
    if (timer) { clearTimeout(timer); timer = null; }
    let d;
    try { d = await api.get(path); } catch (e) { clear(el).append(h("h2", null, t("cluster.title")), notice("bad", e.message)); return; }
    const top = [h("h2", null, t("cluster.title")), h("p", { class: "small muted" }, t("cluster.hint"))];
    if (d.replica) {
      clear(el).append(...top, notice("info", t("cluster.isReplica", { primary: d.replica.primary || d.replica.of })));
      return;
    }
    if (!d.in_network) {
      clear(el).append(...top, notice("info", t("cluster.noNetwork")));
      return;
    }
    if (d.blockers && d.blockers.length) {
      clear(el).append(...top, notice("info", t("cluster.blocked"), h("ul", null, ...d.blockers.map((b) => h("li", null, b)))));
      return;
    }
    const sw = toggle(t(d.clustered ? "cluster.on" : "cluster.off"), { checked: d.clustered });
    sw.input.addEventListener("change", async () => {
      const on = sw.input.checked;
      if (!(await confirm({ title: t(on ? "cluster.onQ" : "cluster.offQ", { domain }), body: t(on ? "cluster.onBody" : "cluster.offBody"), confirmLabel: t(on ? "cluster.onBtn" : "cluster.offBtn"), danger: !on }))) {
        sw.input.checked = !on;
        return;
      }
      patch(on ? { replicas: 1 } : null);
    });
    if (el.childNodes.length === 0 || formFor !== d.clustered) {
      clear(el).append(...top, h("div", { class: "field" }, sw.el), form, rows);
      formFor = d.clustered;
      if (d.clustered) drawForm(d); else clear(form);
    }
    if (d.clustered) drawRows(d); else clear(rows);
    // Live while the page shows it: the rounds are every few seconds.
    timer = setTimeout(() => { if (el.isConnected) load(); }, 5000);
  }
  load();
  return el;
}
