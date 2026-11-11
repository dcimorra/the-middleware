// Every node at a glance: whether it answers, how its machine is doing,
// which of its sites are in trouble, which conversations are at work or
// waiting, and what went wrong lately. One request to this node, which
// asks the others in parallel; read every ten seconds while open. The
// links between nodes are made and undone here too.
//
// It is the page the panel opens on, read every day, so it is ordered by
// what a tired operator needs first: what needs them (or a calm line
// saying nothing does), then each machine, then the network. How linking
// works used to sit in a card of three paragraphs under everything on
// every visit; it is folded now, and a node alone gets one quiet card
// that says how to link another instead of a banner and a header full of
// network buttons it may never use.

import { h, clear, svg, fmtBytes, fmtAgo, copy } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api } from "../lib/api.js";
import { store } from "../lib/store.js";
import { navigate } from "../lib/router.js";
import { currentNode, setCurrentNode } from "../lib/node.js";
import { page, loading, errorBox } from "../components/page.js";
import { open, field, confirm, toggle } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { icon } from "../lib/icons.js";
import { movesCard } from "../components/moves.js";

const pct = (v) => `${Math.round(Number(v || 0))}%`;
const levelOf = (v) => (v >= 90 ? "bad" : v >= 75 ? "warn" : "ok");

// go opens something on a node: the panel moves to that machine first.
function go(node, path) {
  setCurrentNode(node.self ? "" : node.id);
  navigate(path);
}

// spark draws one measure of the hour, a sample a minute. The scale is
// the hour's own, not 0 to 100: on a machine idling at 10% a 0-100 chart
// was a flat line on the floor and said nothing. It never goes under 20%
// either, or a wobble from 2% to 4% would fill the box like a storm; the
// peak is written beside it so the height can be read.
function spark(minutes, k, cls) {
  const W = 300, H = 40, n = minutes.length;
  const vals = minutes.map((m) => Math.min(100, Math.max(0, Number(m[k]) || 0)));
  const top = Math.max(20, Math.ceil((Math.max(...vals) * 1.15) / 10) * 10);
  const y = (v) => (H - 2 - (v / top) * (H - 4)).toFixed(1);
  let d = "";
  vals.forEach((v, i) => { d += (i ? "L" : "M") + ((i / (n - 1)) * W).toFixed(1) + " " + y(v) + " "; });
  return svg("svg", { class: "np-spark " + cls, viewBox: `0 0 ${W} ${H}`, preserveAspectRatio: "none", "aria-hidden": "true" },
    svg("path", { class: "area", d: `${d}L${W} ${H} L0 ${H} Z` }),
    svg("path", { class: "line", d }));
}

// vital is one of the machine's three numbers: the value large, then the
// hour as a line where there is one (processor, memory) or a fill bar
// where a level is what matters (disk), then the detail.
function vital(label, value, detail, minutes, k) {
  const v = Math.min(100, Math.max(0, Number(value || 0)));
  const level = levelOf(v);
  let viz;
  let peak = "";
  if (minutes && minutes.length > 1) {
    viz = spark(minutes, k, level);
    peak = t("dash.peakHour", { pct: pct(Math.max(...minutes.map((m) => Number(m[k]) || 0))) });
  } else {
    const fill = h("div", { class: "dash-bar-fill " + level });
    fill.style.width = v.toFixed(0) + "%";
    viz = h("div", { class: "dash-bar", role: "meter", "aria-valuenow": v.toFixed(0), "aria-valuemin": "0", "aria-valuemax": "100", "aria-label": label }, fill);
  }
  return h("div", { class: "np-vital " + level },
    h("div", { class: "np-vital-head" }, h("span", null, label), h("strong", null, pct(v))),
    viz,
    h("div", { class: "np-vital-foot" }, [detail, peak].filter(Boolean).join(" · ")));
}

function stat(value, label, kind) {
  return h("div", { class: "dash-stat " + (kind || "") }, h("strong", null, String(value)), h("span", null, label));
}

// menu is a node's seldom-used actions folded under "…", as the chat's
// are: five buttons in a card's head wrapped onto a second line and put
// "remove from the network" a slip of the finger from "open".
function menu(items) {
  const list = h("div", { class: "np-menu-list", role: "menu" });
  const el = h("details", { class: "np-menu" },
    h("summary", { class: "icon-btn", "aria-label": t("dash.more"), title: t("dash.more") }, icon("more", { size: 18 })), list);
  for (const it of items) {
    const b = h("button", { type: "button", role: "menuitem", class: "np-menu-item" + (it.danger ? " danger" : ""), title: it.title || null }, it.icon ? icon(it.icon, { size: 16 }) : null, it.label);
    b.addEventListener("click", () => { el.open = false; it.onClick(); });
    list.append(b);
  }
  return el;
}

function nodeCard(node, acts, member, myVersion, openEvents) {
  const s = node.summary || {};
  const viewing = (!node.self && currentNode() === node.id) || (node.self && !currentNode());
  const head = h("div", { class: "dash-node-head" },
    h("span", { class: "dot " + (node.ok ? "ok" : "bad") }),
    h("h2", null, node.name),
    node.self ? h("span", { class: "badge info" }, t("dash.thisNode")) : null,
    viewing ? h("span", { class: "badge accent" }, t("dash.viewing")) : null,
    h("div", { class: "grow" }),
    viewing ? null : h("button", { type: "button", class: "btn sm", title: t("dash.openHint"), onClick: () => go(node, "/sites") }, icon("external"), t("dash.open")),
    acts.length ? menu(acts) : null);
  const card = h("article", { class: "card dash-node np-node" + (node.ok ? "" : " down") }, head);
  const meta = [];
  if (s.hostname && s.hostname !== node.name) meta.push(s.hostname);
  if (s.version) meta.push(s.version);
  if (s.started) meta.push(t("dash.up", { ago: fmtAgo(s.started, t) }));
  if (node.ip) meta.push(node.ip);
  if (node.latency_ms) meta.push(`${node.latency_ms} ms`);
  if (meta.length) card.append(h("p", { class: "dash-meta" }, meta.join(" · ")));
  if (member) {
    const hs = member.handshake && !member.handshake.startsWith("0001") ? member.handshake : "";
    card.append(h("p", { class: "dash-tunnel" }, h("span", { class: "dot " + (member.online ? "ok" : "bad") }),
      hs ? t("dash.tunnelSeen", { ago: fmtAgo(hs, t) }) : t("dash.tunnelNever"),
      h("span", null, " · " + (member.endpoint || t("dash.behindNAT")))));
  }
  if (!node.ok) {
    card.append(h("div", { class: "np-node-down" }, icon("ban", { size: 18 }), h("div", null, h("strong", null, t("dash.unreachable")), " ", h("span", { class: "mono" }, node.error || ""))));
    return card;
  }
  const v = s.vitals;
  if (v && v.now) {
    const n = v.now;
    const mins = v.minutes || [];
    card.append(h("div", { class: "np-vitals" },
      vital(t("vitals.cpu"), n.cpu, t("dash.cores", { n: n.cores || 0, load: Number(n.load1 || 0).toFixed(2) }), mins, 1),
      vital(t("vitals.memory"), n.mem_pct, `${fmtBytes(n.mem_used)} / ${fmtBytes(n.mem_total)}`, mins, 2),
      vital(t("vitals.disk"), n.disk_pct, `${fmtBytes(n.disk_used)} / ${fmtBytes(n.disk_total)}`)));
  }
  const sc = s.sites;
  if (sc) {
    const chats = s.chats || { asking: [], working: 0 };
    // The counts as one line of words: trouble and waiting are said again,
    // with names, in what needs attention above, so here they are counts.
    const part = (n, label, kind) => h("span", { class: "np-count " + (kind || "") }, h("b", null, String(n)), " ", label);
    const parts = [part(sc.serving, t("dash.serving"), "ok")];
    if (sc.trouble.length) parts.push(part(sc.trouble.length, t("dash.trouble"), "bad"));
    if (sc.stopped) parts.push(part(sc.stopped, t("dash.stopped")));
    if (sc.projects) parts.push(part(sc.projects, t("dash.projects")));
    if (chats.working) parts.push(part(chats.working, t("dash.working"), "accent"));
    if ((chats.asking || []).length) parts.push(part(chats.asking.length, t("dash.asking"), "warn"));
    card.append(h("a", { class: "np-node-sites", href: "#/sites", onClick: (e) => { e.preventDefault(); go(node, "/sites"); } },
      icon("globe", { size: 16 }), h("span", { class: "np-counts" }, ...parts), icon("external", { size: 14, class: "np-go" })));
  }
  const all = s.events || [];
  const evs = all.slice(0, 4);
  if (evs.length) {
    const det = h("details", { class: "np-events", open: openEvents.has(node.id) },
      h("summary", null, t("dash.recentTrouble", { n: all.length })),
      h("ul", { class: "np-event-list" }, ...evs.map((e) => h("li", null,
        h("span", { class: "dot " + (e.level === "error" ? "bad" : "warn") }),
        h("span", { class: "grow" }, e.subject ? h("strong", null, e.subject + " ") : null, e.message || e.type),
        h("span", { class: "muted nowrap", title: e.time }, fmtAgo(e.time, t))))));
    // The card is drawn again every ten seconds: a list the operator
    // opened must still be open after it.
    det.addEventListener("toggle", () => { if (det.open) openEvents.add(node.id); else openEvents.delete(node.id); });
    card.append(det);
  }
  return card;
}

// attention gathers, from every node, what someone has to act on, worst
// first: a node that does not answer, a site down, a conversation waiting
// for an answer, a machine under strain or with its disk full, two nodes
// on different versions, an update. Each line goes where it is fixed.
function attentionItems(cards, info, many) {
  const items = [];
  const add = (kind, node, title, sub, onClick) => items.push({ kind, node: many && node ? node.name : "", title, sub, onClick });
  if (info && info.network && info.error) add("bad", null, t("dash.meshError"), info.error);
  const mine = cards[0] && cards[0].summary && cards[0].summary.version;
  for (const c of cards) {
    const s = c.summary || {};
    if (!c.ok) { add("bad", c, t("dash.nodeDown", { name: c.name }), c.error || ""); continue; }
    for (const x of (s.sites && s.sites.trouble) || []) add("bad", c, x.domain, t("dash.why." + x.why), () => go(c, `/sites/${encodeURIComponent(x.domain)}`));
    for (const ch of (s.chats && s.chats.asking) || []) {
      add("warn", c, ch.name || ch.domain, t("dash.waitsForYou"), () => go(c, ch.domain && ch.domain !== "middleware" ? `/sites/${encodeURIComponent(ch.domain)}/chat` : "/house"));
    }
    const v = s.vitals;
    if (v && v.open_episode) {
      const e = v.open_episode;
      const who = (e.blamed || []).map((b) => b.site || b.cmd).join(", ");
      add("warn", c, t("dash.strain", { reason: t("vitals.reason." + e.reason) }), who ? t("dash.strainWho", { who }) : "", () => go(c, "/system"));
    }
    if (v && v.now && Number(v.now.disk_pct) >= 90) {
      add("warn", c, t("dash.diskFull", { pct: pct(v.now.disk_pct) }), t("dash.diskFullHint", { used: fmtBytes(v.now.disk_used), total: fmtBytes(v.now.disk_total) }), () => go(c, "/system"));
    }
    if (s.version && mine && s.version !== mine) add("warn", c, t("dash.otherVersion", { v: s.version, mine }), "");
    if (s.update && s.update.available) add("info", c, t("dash.update", { v: s.update.latest }), t("dash.updateHint"), () => go(c, "/system"));
  }
  // Worst first, whichever node it is on; within a kind, the nodes' order.
  const rank = { bad: 0, warn: 1, info: 2 };
  return items.map((it, i) => [it, i]).sort((a, b) => rank[a[0].kind] - rank[b[0].kind] || a[1] - b[1]).map(([it]) => it);
}

function attentionCard(items) {
  if (!items.length) {
    return h("div", { class: "np-calm" }, h("span", { class: "np-calm-mark" }, icon("check", { size: 18 })),
      h("div", null, h("strong", null, t("dash.allClear")), h("span", null, t("dash.allClearHint"))));
  }
  const list = h("ul", { class: "np-attn-list" });
  for (const it of items) {
    const body = [
      h("span", { class: "np-attn-mark " + it.kind }, icon(it.kind === "bad" ? "alert" : it.kind === "warn" ? "bell" : "info", { size: 16 })),
      h("span", { class: "np-attn-text" }, h("strong", null, it.title, it.node ? h("span", { class: "badge" }, it.node) : null), it.sub ? h("span", null, it.sub) : null),
      it.onClick ? icon("external", { size: 14, class: "np-go" }) : null,
    ];
    list.append(h("li", null, it.onClick
      ? h("button", { type: "button", class: "np-attn-row", onClick: it.onClick }, ...body)
      : h("div", { class: "np-attn-row" }, ...body)));
  }
  return h("section", { class: "card np-attn" },
    h("h2", null, t("dash.attention"), h("span", { class: "np-count-pill" }, String(items.length))), list);
}

function howFold(...more) {
  return h("details", { class: "np-fold" }, h("summary", null, t("dash.howTitle")),
    h("div", { class: "np-fold-body" }, h("p", null, t("dash.how1")), h("p", null, t("dash.how2")), h("p", null, t("dash.how3")), ...more));
}

export function dashboardScreen() {
  const joinBtn = h("button", { type: "button", class: "btn", hidden: true }, icon("link"), t("dash.join"));
  const inviteBtn = h("button", { type: "button", class: "btn primary", hidden: true }, icon("plus"), t("dash.invite"));
  const p = page({ title: t("nav.dashboard"), icon: "layout", sub: t("dash.sub"), actions: [joinBtn, inviteBtn] });
  p.el.classList.add("np-page");
  const attention = h("div");
  const totals = h("div", { class: "dash-totals" });
  const grid = h("div", { class: "dash-grid np-grid" }, loading());
  // The network's settings when there is one; the way to make one when not.
  const netCard = h("div", { class: "card", hidden: true });
  const linkCard = h("div", { class: "card np-link", hidden: true });
  const moves = movesCard("");
  p.body.append(attention, totals, grid, moves.el, netCard, linkCard);
  let cards = [];
  let info = null;
  let alive = true;
  const openEvents = new Set();

  function paint() {
    const inNet = Boolean(info && info.network);
    joinBtn.hidden = true;
    inviteBtn.hidden = !inNet;
    const many = cards.length > 1;
    clear(attention).append(attentionCard(attentionItems(cards, info, many)));
    paintNet();
    paintLink();
    clear(totals);
    // One node's totals are its own card's numbers: the strip is for a
    // network, where it adds the machines up.
    if (many) {
      const ok = cards.filter((c) => c.ok);
      const sum = (f) => ok.reduce((n, c) => n + (f(c.summary || {}) || 0), 0);
      totals.append(
        stat(`${ok.length}/${cards.length}`, t("dash.nodesUp"), ok.length < cards.length ? "bad" : "ok"),
        stat(sum((s) => s.sites && s.sites.sites), t("dash.sites")),
        stat(sum((s) => s.chats && s.chats.working), t("dash.working"), sum((s) => s.chats && s.chats.working) ? "accent" : ""));
    }
    clear(grid);
    const byID = new Map(((info && info.members) || []).map((m) => [m.id, m]));
    const mine = cards[0] && cards[0].summary && cards[0].summary.version;
    for (const c of cards) {
      const acts = [];
      if (c.self) acts.push({ icon: "edit", label: t("dash.rename"), onClick: () => rename(c) });
      // A node in a network can drop its panel's domain and be managed
      // from here alone: its record and route retired, nothing public.
      if (inNet && c.ok && c.summary && c.summary.panel_domain) {
        acts.push({ icon: "lock", label: t("dash.networkOnly"), title: t("dash.networkOnlyHint"), onClick: () => networkOnly(c) });
      }
      if (c.self) { if (inNet) acts.push({ icon: "logout", label: t("dash.leave"), danger: true, onClick: leave }); }
      else acts.push({ icon: "trash", label: t("dash.expel"), danger: true, onClick: () => expel(c) });
      grid.append(nodeCard(c, acts, byID.get(c.id), mine, openEvents));
    }
  }

  function paintNet() {
    netCard.hidden = !(info && info.network);
    if (netCard.hidden) return;
    const on = Boolean(info.settings && info.settings.panel_vpn_only);
    const sw = toggle(t("dash.panelVPNOnly"), { checked: on });
    sw.input.addEventListener("change", async () => {
      const want = sw.input.checked;
      if (want && !(await confirm({ title: t("dash.panelVPNOnlyQ"), body: t("dash.panelVPNOnlyBody"), confirmLabel: t("dash.panelVPNOnlyGo") }))) { sw.input.checked = false; return; }
      try {
        await api.put("/nodes/settings", { panel_vpn_only: want }, { node: "" });
        toast(t(want ? "dash.panelVPNOnlyOn" : "dash.panelVPNOnlyOff"), "ok");
        load();
      } catch (e) { sw.input.checked = !want; toastError(e); }
    });
    const wasOpen = netCard.querySelector("details[open]");
    // How the relay works is how linking works: read once, then folded
    // with the rest of it.
    const how = howFold(h("p", null, t("dash.relayHint")));
    how.open = Boolean(wasOpen);
    clear(netCard).append(
      h("h2", null, t("dash.netTitle")),
      h("div", { class: "field" }, sw.el, h("p", { class: "hint" }, t("dash.panelVPNOnlyHint"))),
      how);
  }

  // A node alone: one card at the bottom with the two ways to stop being
  // alone, and how it works folded under them.
  function paintLink() {
    linkCard.hidden = !info || Boolean(info.network);
    if (linkCard.hidden || linkCard.firstChild) return;
    const invite = h("button", { type: "button", class: "btn primary" }, icon("plus"), t("dash.invite"));
    const join = h("button", { type: "button", class: "btn" }, icon("link"), t("dash.join"));
    invite.addEventListener("click", () => inviteBtn.click());
    join.addEventListener("click", () => joinBtn.click());
    linkCard.append(
      h("div", { class: "np-link-head" }, h("span", { class: "np-link-icon" }, icon("link")),
        h("div", { class: "grow" }, h("h2", null, t("dash.linkTitle")), h("p", null, t("dash.aloneHint"))),
        h("div", { class: "actions" }, join, invite)),
      howFold());
  }

  async function load() {
    // A menu left open would be closed under the operator's finger by the
    // redraw: the next tick draws it.
    if (cards.length && grid.querySelector(".np-menu[open]")) return;
    try {
      [cards, info] = await Promise.all([api.get("/nodes/overview", { node: "" }), api.get("/nodes", { node: "" })]);
      store.set("nodes", info);
      paint();
    } catch (e) {
      if (!cards.length) clear(grid).append(errorBox(e));
    }
  }

  async function rename(c) {
    const name = h("input", { type: "text", value: c.name, maxlength: "40", required: true });
    const done = await open({
      title: t("dash.renameTitle"),
      body: [field(t("dash.name"), name, t("dash.nameHint"))],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("common.save"), submit: true, primary: true }],
      onSubmit: async () => { await api.put("/nodes/self", { name: name.value }, { node: "" }); return true; },
    });
    if (done) load();
  }

  async function networkOnly(c) {
    const domain = c.summary.panel_domain;
    const here = c.self && location.hostname === domain;
    const ok = await confirm({ title: t("dash.networkOnlyQ", { name: c.name, domain }), body: t(here ? "dash.networkOnlyBodyHere" : "dash.networkOnlyBody", { domain }), confirmLabel: t("dash.networkOnly"), danger: here });
    if (!ok) return;
    try {
      const r = await api.at(c.self ? "" : c.id).post("/system/panel/network-only");
      toast(t("dash.networkOnlyDone", { name: c.name, from: (r.manage_from || []).join(", ") }), "ok");
      load();
    } catch (e) { toastError(e); }
  }

  async function expel(c) {
    const ok = await confirm({ title: t("dash.expelQ", { name: c.name }), body: t("dash.expelBody"), confirmLabel: t("dash.expel"), danger: true });
    if (!ok) return;
    try {
      await api.del(`/nodes/${encodeURIComponent(c.id)}`, { node: "" });
      toast(t("dash.expelled", { name: c.name }), "ok");
      if (currentNode() === c.id) setCurrentNode("");
      load();
    } catch (e) { toastError(e); }
  }

  async function leave() {
    const ok = await confirm({ title: t("dash.leaveQ"), body: t("dash.leaveBody"), confirmLabel: t("dash.leave"), danger: true });
    if (!ok) return;
    try {
      await api.post("/nodes/leave", {}, { node: "" });
      setCurrentNode("");
      toast(t("dash.left"), "ok");
      load();
    } catch (e) { toastError(e); }
  }

  inviteBtn.addEventListener("click", async () => {
    const creating = !(info && info.network);
    const endpoint = h("input", { type: "text", value: (info && info.suggested_endpoint) || "", placeholder: "203.0.113.10:51821", autocomplete: "off", spellcheck: "false" });
    const out = h("div", { class: "stack-sm" });
    await open({
      title: t(creating ? "dash.createTitle" : "dash.inviteTitle"),
      wide: true,
      body: [
        h("p", { class: "dim" }, t(creating ? "dash.createBody" : "dash.inviteBody")),
        creating ? field(t("dash.endpoint"), endpoint, t("dash.endpointHint")) : null,
        out,
      ],
      buttons: [{ label: t("common.close"), value: null }, { label: t("dash.makeCode"), submit: true, primary: true }],
      onSubmit: async () => {
        const r = await api.post("/nodes/invite", { endpoint: creating ? endpoint.value : "" }, { node: "" });
        const box = h("textarea", { class: "mono code-box", rows: "4", readonly: true }, r.code);
        const cp = h("button", { type: "button", class: "btn sm" }, icon("copy"), t("common.copy"));
        cp.addEventListener("click", async () => { if (await copy(r.code)) toast(t("common.copied"), "ok"); });
        const row = h("div", { class: "row" }, cp);
        const more = [];
        // For a server that is being rented now: the same code inside a
        // cloud-init file. Where the page cannot write to the clipboard
        // (plain http through an SSH tunnel), the file takes the box's
        // place to be selected by hand.
        if (r.cloud_config) {
          const cc = h("button", { type: "button", class: "btn sm" }, icon("copy"), t("dash.copyCloudConfig"));
          cc.addEventListener("click", async () => {
            if (await copy(r.cloud_config)) { toast(t("common.copied"), "ok"); return; }
            box.value = r.cloud_config;
            box.rows = 12;
            box.select();
            toast(t("common.copyFailed"), "bad");
          });
          row.append(cc);
          more.push(h("p", { class: "small muted" }, t("dash.cloudConfigHint")));
        }
        row.append(h("span", { class: "small muted" }, t("dash.codeExpires", { at: new Date(r.expires).toLocaleTimeString() })));
        clear(out).append(box, row, ...more);
        box.select();
        load();
        return false;
      },
    });
  });

  joinBtn.addEventListener("click", async () => {
    const code = h("textarea", { class: "mono code-box", rows: "4", placeholder: "mwmesh_…", spellcheck: "false", required: true });
    const endpoint = h("input", { type: "text", value: (info && info.suggested_endpoint) || "", placeholder: "203.0.113.11:51821", autocomplete: "off", spellcheck: "false" });
    const made = await open({
      title: t("dash.joinTitle"),
      wide: true,
      body: [h("p", { class: "dim" }, t("dash.joinBody")), field(t("dash.code"), code), field(t("dash.endpoint"), endpoint, t("dash.joinEndpointHint"))],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("dash.join"), submit: true, primary: true }],
      onSubmit: async (_f, showError) => {
        if (!code.value.trim()) { showError(t("dash.codeNeeded")); return false; }
        return api.post("/nodes/join", { code: code.value.trim(), endpoint: endpoint.value.trim() }, { node: "" });
      },
    });
    if (made) { toast(t("dash.joined", { name: made.inviter }), "ok"); load(); }
  });

  // A tap anywhere else closes a node's menu, as a menu does.
  const closeMenus = (e) => { for (const m of grid.querySelectorAll(".np-menu[open]")) if (!m.contains(e.target)) m.open = false; };
  document.addEventListener("click", closeMenus);

  load();
  const timer = setInterval(() => { if (alive && !document.hidden) load(); }, 10000);
  return { el: p.el, destroy() { alive = false; clearInterval(timer); moves.destroy(); document.removeEventListener("click", closeMenus); } };
}
