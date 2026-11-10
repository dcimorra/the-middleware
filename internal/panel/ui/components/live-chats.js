// The conversations, in the bar, from any screen: the last used on every
// node of the network, a click away, and among them the ones at work and
// the ones waiting for an answer. As in V1: a turn can last twenty
// minutes, and the only way to know it was alive was to remember which
// site it was left in; a conversation that ended asking something looked
// finished and its question waited inside a site nobody was going to
// open. What the button says is whose turn it is: someone waiting, if
// anyone is.
//
// It only showed while something was at work, and coming back to a
// conversation that had ended meant the node, the site, its chat and the
// list of that site's conversations. So it is always there, with the last
// LIMIT used, and the operator's list ends with the purge of all of them.

import { h, clear, fmtAgo } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api } from "../lib/api.js";
import { store } from "../lib/store.js";
import { navigate } from "../lib/router.js";
import { setCurrentNode, currentNode } from "../lib/node.js";
import { onEvent } from "../lib/stream.js";
import { delegate } from "../lib/access.js";
import { icon } from "../lib/icons.js";
import { confirm } from "./dialog.js";
import { toast } from "./toast.js";

const LIMIT = 15;

export function liveChats({ onCount } = {}) {
  const count = h("span", { class: "live-count" });
  const lead = h("span", { class: "live-lead" });
  const btn = h("button", { type: "button", class: "live-btn", "aria-haspopup": "true", "aria-expanded": "false" }, lead, count);
  const drop = h("div", { class: "live-drop", role: "menu", hidden: true });
  const el = h("div", { class: "live-chats", hidden: true }, btn, drop);
  let rows = [];

  function close() { drop.hidden = true; btn.setAttribute("aria-expanded", "false"); }
  btn.addEventListener("click", (e) => {
    e.stopPropagation();
    drop.hidden = !drop.hidden;
    btn.setAttribute("aria-expanded", String(!drop.hidden));
  });
  document.addEventListener("click", (e) => { if (!el.contains(e.target)) close(); });
  document.addEventListener("keydown", (e) => { if (e.key === "Escape") close(); });

  function open(r) {
    close();
    // The conversation itself, not the site's last one: the chat screen
    // reads this once when it opens.
    store.set("openChat", { domain: r.domain, id: r.id || "" });
    if ((currentNode() || "") !== r.node) setCurrentNode(r.node);
    // The house's own, the network agent's and a crew agent's sessions
    // live on their own pages, not under a site.
    const where = r.domain === "middleware" ? "/house"
      : r.domain === "autopilot" ? "/autopilot/sessions"
        : r.domain.startsWith("crew~") ? `/agents/${encodeURIComponent(r.domain.slice(5))}/sessions`
          : `/sites/${encodeURIComponent(r.domain)}/chat`;
    navigate(where);
  }

  // Every node the operator's list names, each asked on its own: one that
  // does not answer leaves its chats out, not the others'.
  function targets() {
    const n = store.get("nodes");
    return [{ id: "", name: (n && n.self && n.self.name) || "" }, ...(((n && n.members) || []).filter((m) => m.online !== false).map((m) => ({ id: m.id, name: m.name })))];
  }

  async function purgeAll() {
    close();
    const nodes = targets();
    if (!(await confirm({ title: t("live.purgeQ"), body: t(nodes.length > 1 ? "live.purgeBodyNodes" : "live.purgeBody", { n: nodes.length }), confirmLabel: t("live.purge"), danger: true }))) return;
    let purged = 0, busy = 0, failed = 0;
    await Promise.all(nodes.map(async (x) => {
      try {
        const r = await api.at(x.id).del("/agents/chats");
        purged += (r && r.purged) || 0;
        busy += (r && r.busy) || 0;
      } catch { failed++; }
    }));
    const parts = [t("chat.purged", { n: purged })];
    if (busy) parts.push(t("live.purgeBusy", { n: busy }));
    if (failed) parts.push(t("live.purgeFailed", { n: failed }));
    toast(parts.join(" · "), failed ? "bad" : "ok");
    load();
  }

  function paint() {
    const live = rows.filter((r) => r.asking || r.running);
    const asking = live.filter((r) => r.asking).length;
    const talking = live.length - asking;
    if (onCount) onCount(asking, talking);
    el.hidden = false;
    el.dataset.state = asking ? "asking" : talking ? "talking" : "idle";
    const label = asking ? t("live.waiting", { n: asking }) : talking ? t("live.talking", { n: talking }) : t("live.recent");
    clear(lead).append(asking || talking ? h("span", { class: "live-dot" }) : icon("chat"));
    clear(count).append(
      h("span", { class: "long" }, label),
      h("span", { class: "short" }, asking || talking ? String(asking || talking) : ""));
    btn.title = label;
    btn.setAttribute("aria-label", label);
    // In a network every row says its node, whether or not the others
    // have a conversation in the list now: one list by last use rather
    // than a group per node, so the one just used is always on top.
    const network = !delegate() && (((store.get("nodes") || {}).members) || []).length > 0;
    const items = rows.map((r) => {
      const item = h("button", { type: "button", class: "live-item" + (r.asking ? " asking" : ""), role: "menuitem" },
        h("span", { class: "live-mark" + (r.asking ? " asking" : r.running ? " talking" : "") }),
        h("span", { class: "live-main" },
          h("span", { class: "live-name" },
            h("b", null, r.domain === "middleware" ? t("nav.house") : r.domain),
            network && r.nodeName ? h("span", { class: "live-node" }, r.nodeName) : null),
          h("small", null, r.title || t("chat.untitled"))),
        h("span", { class: "live-side" }, r.asking ? t("live.asks") : r.running ? t("live.talkingSince", { ago: fmtAgo(r.updated, t) }) : fmtAgo(r.updated, t)));
      item.addEventListener("click", () => open(r));
      return item;
    });
    clear(drop).append(h("div", { class: "live-head" }, t("live.recentTitle")),
      ...(items.length ? items : [h("p", { class: "live-empty" }, t("live.empty"))]));
    // Every conversation of every node is the operator's to forget; a
    // delegate's list is their sites', and their own site's chat has its
    // own purge.
    if (!delegate() && rows.length) {
      const purge = h("button", { type: "button", class: "live-item live-purge", role: "menuitem" }, icon("trash"), h("span", null, t("live.purge")));
      purge.addEventListener("click", purgeAll);
      drop.append(h("div", { class: "live-sep", role: "separator" }), purge);
    }
  }

  // The operator asks each node for its last used; a node that does not
  // know the question yet (an older version in the network) is asked the
  // old one, whose answer is only what is alive.
  async function fromNode(x) {
    const a = api.at(x.id);
    try {
      return (await a.get(`/agents/recent?limit=${LIMIT}`)) || [];
    } catch (e) {
      if (e.status !== 404) return [];
    }
    try {
      const w = await a.get("/agents/waiting");
      return [...(w.asking || []).map((c) => ({ ...c, asking: true })), ...(w.running || []).map((c) => ({ ...c, running: true }))];
    } catch { return []; }
  }

  // A delegate sees the conversations of the sites where the chat is
  // theirs, asked site by site: the house's list names every site.
  async function fromSites() {
    const me = store.get("me");
    const sites = ((me && me.user && me.user.sites) || []).filter((s) => (s.perms || []).includes("agents"));
    const lists = await Promise.all(sites.map(async (s) => {
      try {
        const cs = await api.get(`/sites/${encodeURIComponent(s.site)}/chats?main=1`);
        return (cs || []).filter((c) => c.origin !== "site" || c.running).map((c) => ({ ...c, domain: s.site }));
      } catch { return []; }
    }));
    return lists.flat();
  }

  async function load() {
    const next = [];
    if (delegate()) {
      for (const c of await fromSites()) next.push({ ...c, node: "", nodeName: "" });
    } else {
      const answers = await Promise.all(targets().map(async (x) => ({ x, cs: await fromNode(x) })));
      for (const { x, cs } of answers) for (const c of cs) next.push({ ...c, node: x.id, nodeName: x.name });
    }
    // The last used first, whatever its state: the way back to a
    // conversation is where it was left. The ones at work and waiting
    // stay past the limit, since the button counts them.
    next.sort((a, b) => new Date(b.updated) - new Date(a.updated));
    rows = next.filter((r, i) => i < LIMIT || r.asking || r.running);
    // Shared, so a screen can say the same at its own place: the sites
    // list marks each site's chat button with whose turn it is.
    store.set("liveChats", rows.filter((r) => r.asking || r.running));
    paint();
  }

  let timer = null;
  const soon = () => { clearTimeout(timer); timer = setTimeout(load, 400); };
  onEvent("agent.", (ev) => { if (/^agent\.(done|error|question|started)$/.test(ev.type)) soon(); });
  onEvent("chat.", (ev) => { if (/^chat\.(purged|reset)$/.test(ev.type)) soon(); });
  store.subscribe("nodes", soon);
  // The bar asks every node; a panel forgotten in a background tab did so
  // every 20 seconds for days. Hidden, it waits, and catches up the
  // moment the tab is shown again.
  setInterval(() => { if (!document.hidden) load(); }, 20000);
  document.addEventListener("visibilitychange", () => { if (!document.hidden) soon(); });
  return { el, load };
}
