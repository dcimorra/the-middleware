// One site: header with status, tabs, each tab its own module. The header
// re-renders from the store so a restart pressed here shows "handing
// over" within the tick that does it, from the same event the list sees.

import { h, clear, append, shortRev, fmtTime, fmtAgo, fmtBytes, busy } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, apiURL, enc } from "../lib/api.js";
import { currentNode } from "../lib/node.js";
import { delegate, can } from "../lib/access.js";
import { onEvent } from "../lib/stream.js";
import { store } from "../lib/store.js";
import { navigate } from "../lib/router.js";
import { page, table, cell, empty, loading, errorBox, notice } from "../components/page.js";
import { icon } from "../lib/icons.js";

import { modeBadge, aliveStatus, siteMode } from "../components/badge.js";
import { tabs } from "../components/tabs.js";
import { open, confirm, field, checkbox, toggle } from "../components/dialog.js";
import { menu } from "./site-menu.js";
import { toast, toastError } from "../components/toast.js";
import { jsonEditor } from "../components/editor.js";
import { movesCard, openMove } from "../components/moves.js";
import { logsTab } from "./site-logs.js";
import { filesTab } from "./site-files.js";
import { chatTab } from "./site-chat.js";
import { accessTab } from "./site-access.js";
import { securityTab } from "./site-security.js";
import { activityTab } from "./site-activity.js";
import { cacheTab } from "./site-cache.js";
import { siteTasksTab } from "./tasks.js";
import { doorCard } from "./site-door.js";
import { specsTab } from "./site-specs.js";

// No browser to take pictures with on this machine: learnt from the
// first refusal and kept for the visit, so every overview after it says
// so instead of offering a button that fails.
let shotsOff = false;

const TABS = ["overview", "activity", "manifest", "logs", "files", "chat", "tasks", "specs", "access", "security", "cache", "moves"];
const TAB_ICON = { overview: "gauge", activity: "activity", manifest: "code", logs: "list", files: "folder", chat: "chat", tasks: "clock", specs: "file", access: "lock", security: "shield", cache: "box", moves: "upload" };

// The tabs a delegated user sees, by what opens them: the WAF, the edge
// cache, the house's guard and the moves are the operator's, and the
// access tab writes the manifest.
const DELEGATE_TABS = { overview: "view", manifest: "view", logs: "view", files: "view", tasks: "view", specs: "view", chat: "agents", access: "code" };

export function siteScreen({ domain, tab, query }) {
  const shownTabs = delegate() ? TABS.filter((k) => DELEGATE_TABS[k] && can(domain, DELEGATE_TABS[k])) : TABS;
  const active = shownTabs.includes(tab) ? tab : "overview";
  // ?worker=name opens the logs or the activity on that worker (the
  // workers card links there).
  const pickWorker = (query && query.get("worker")) || "";
  // ?service=name (and &build=1) does the same for a service.
  const pickService = (query && query.get("service")) || "";
  const pickLog = pickService ? (query.get("build") ? "build:" : "service:") + pickService : pickWorker;
  const base = `#/sites/${enc(domain)}`;
  const statusLine = h("div", { class: "site-head-state" });
  // The crumb is the way back, not a second copy of the title beside it.
  const p = page({
    title: domain,
    crumbs: [h("a", { href: "#/sites" }, t("nav.sites"))],
    actions: statusLine,
  });
  p.head.classList.add("site-head");
  // The site's own name for itself (manifest title), quiet under the
  // domain: "Shop" says which one this is faster than the domain does.
  const sub = h("p", { class: "sub site-head-title", hidden: true });
  p.head.append(sub);
  const nav = tabs(shownTabs.map((k) => ({ key: k, href: `${base}/${k}`, label: t("site.tab." + k), icon: TAB_ICON[k] })), active);
  nav.classList.add("site-tabs");
  p.body.append(nav);
  const body = h("div", { class: "site-body" });
  p.body.append(body);
  const subs = [];
  let view = null;
  subs.push(tabsScroller(nav));

  function renderStatus() {
    const s = store.site(domain);
    clear(statusLine);
    if (!s) return;
    const mode = siteMode(s);
    const title = (s.manifest && s.manifest.title) || "";
    sub.textContent = title && title !== domain ? title : "";
    sub.hidden = !sub.textContent;
    // The state in words, in a soft pill; the mode beside it only when it
    // says something the state does not ("held" twice read as a stutter).
    statusLine.append(h("span", { class: "site-pill" }, aliveStatus(s)));
    if (!["hold", "stopped", "unmanaged"].includes(mode)) statusLine.append(modeBadge(s));
    // A project has no name to open, and an unmanaged folder is not
    // served: the link went to https://<project> and nowhere.
    if (mode !== "unmanaged" && mode !== "project") {
      const names = openableNames(s, domain);
      if (names.length > 1) {
        // A site with services answers several names (cms.bakery.test,
        // bakery.test/api): one link went only to the site's own, and the
        // others had to be typed by hand.
        statusLine.append(menu(t("sitepage.open_which"),
          names.map((n) => ({ label: n.name, title: n.name, hint: n.service, icon: "external", href: `https://${n.name}`, external: true })),
          { text: t("sitepage.open"), icon: "chevronDown", class: "site-open-menu" }));
      } else {
        statusLine.append(h("a", { class: "btn sm", href: `https://${names.length ? names[0].name : domain}`, target: "_blank", rel: "noopener" }, t("sitepage.open"), icon("external", { size: 14 })));
      }
    }
  }

  function mountTab() {
    if (view && view.destroy) view.destroy();
    clear(body);
    const s = store.site(domain);
    if (!s) return;
    switch (active) {
      case "manifest": view = manifestTab(domain); break;
      case "logs": view = logsTab(domain, pickLog); break;
      case "files": view = filesTab(domain); break;
      case "chat": view = chatTab(domain); break;
      case "tasks": view = siteTasksTab(domain); break;
      case "specs": view = specsTab(domain); break;
      case "access": view = accessTab(domain); break;
      case "security": view = securityTab(domain); break;
      case "activity": view = activityTab(domain, pickWorker); break;
      case "cache": view = cacheTab(domain); break;
      // Its own tab, the last: a move's operations are read when one is
      // under way or went wrong, not every time the site is opened.
      case "moves": view = movesCard(domain, { empty: true }); break;
      default: view = overviewTab(domain);
    }
    body.append(view.el);
  }

  subs.push(store.subscribe("site:" + domain, renderStatus));
  (async () => {
    if (!store.site(domain)) {
      body.append(loading());
      try {
        store.putSite(await api.get(`/sites/${enc(domain)}`));
      } catch (e) {
        clear(body).append(errorBox(e));
        return;
      }
    }
    renderStatus();
    mountTab();
  })();

  return { el: p.el, destroy() { subs.forEach((u) => u()); if (view && view.destroy) view.destroy(); } };
}

// Eleven tabs do not fit a phone, nor a laptop with the sidebar open, and
// the bar scrolls sideways with its scrollbar hidden: the tab that was
// open could sit off screen (Moves always did) with nothing saying there
// was more. The open tab is brought into view, and each edge fades while
// there is more beyond it.
function tabsScroller(nav) {
  const paint = () => {
    const max = nav.scrollWidth - nav.clientWidth;
    nav.classList.toggle("more-left", nav.scrollLeft > 2);
    nav.classList.toggle("more-right", nav.scrollLeft < max - 2);
  };
  const center = () => {
    const a = nav.querySelector('[aria-current="page"]');
    if (a) nav.scrollLeft = a.offsetLeft - (nav.clientWidth - a.offsetWidth) / 2;
    paint();
  };
  nav.addEventListener("scroll", paint, { passive: true });
  const ro = new ResizeObserver(paint);
  ro.observe(nav);
  requestAnimationFrame(center);
  return () => ro.disconnect();
}

// ---------- overview ----------

function overviewTab(domain) {
  const el = h("div");
  const subs = [];
  const envBox = h("div", { class: "site-env-body" });
  const envCount = h("span", { class: "small muted" });
  // The environment is read when something goes wrong, not every time
  // the site is opened: folded, with how many variables it holds in the
  // line that unfolds it. Built once, so a re-render keeps it open.
  const envCard = h("details", { class: "card site-env" },
    h("summary", null, h("h2", null, t("site.env")), envCount, icon("chevronDown", { size: 18, class: "site-env-chev" })),
    h("p", { class: "small muted" }, t("site.envHint")), envBox);
  // Built once: the overview re-renders on every store change, and the
  // door's form must not lose what is being typed into it.
  // The gateway spends the operator's money: theirs alone.
  const owner = !delegate();
  const door = owner ? doorCard(domain) : null;
  // The names the house published for this site, from GET /publish:
  // [name, record] pairs, refreshed on publish.* events.
  let published = [];
  let publishedLoaded = false;
  // The other nodes of this one's network, online: where the site can be
  // moved. Empty without a network, and the button is not shown.
  let otherNodes = [];
  (async () => {
    if (!owner) return;
    try {
      const info = await api.get("/nodes");
      // /nodes is always this panel's own, so "self" is the panel's node,
      // not the site's: with another node chosen, the site lives there, and
      // the list offered that node and left this one out.
      const here = currentNode() || info.self.id;
      // members leaves the panel's own node out (it is in self), and that
      // node is a fine target when the site lives on another one.
      const panelNode = currentNode() && info.network ? [info.self] : [];
      otherNodes = [...panelNode, ...(info.members || []).filter((x) => x.id !== here && x.online && !x.removed)];
      if (otherNodes.length) render();
    } catch { /* no network: nothing to move to */ }
  })();
  async function loadPublished() {
    if (!owner) return;
    try {
      const recs = await api.get("/publish");
      published = Object.entries(recs || {}).filter(([, r]) => r.site === domain).sort(([a], [b]) => a.localeCompare(b));
    } catch { published = []; }
    publishedLoaded = true;
    render();
  }
  subs.push(onEvent("publish.", () => loadPublished()));

  // The names the site answers to: its own, the www of an apex (published
  // and certified, and sent to the apex), and the extra domains, added and
  // taken away here. Each one is published like the site's own name; one
  // the zone already points elsewhere waits for a yes in the list above.
  function domainsCard(m) {
    const aliases = [...(m.aliases || [])];
    const apex = domain.split(".").length === 2;
    const save = async (next, www) => {
      const patch = { aliases: next.length ? next : null };
      if (www !== undefined) patch.www = www ? null : false;
      try {
        await api.patch(`/sites/${enc(domain)}/manifest`, patch);
        toast(t("site.domainsSaved"), "ok");
      } catch (e) { toastError(e); }
    };
    // One line per name, the main one first and marked, each alias with
    // its own × at the end of its line; the www of an apex is a switch on
    // a line of its own, since it is a yes or no and not a name to type.
    const rows = [h("li", { class: "site-addr" }, icon("globe", { size: 16 }), h("code", { class: "grow" }, domain), h("span", { class: "badge" }, t("site.domainMain")))];
    for (const a of aliases) {
      const x = h("button", { type: "button", class: "icon-btn sm", "aria-label": t("site.domainRemove", { name: a }), title: t("site.domainRemove", { name: a }) }, icon("x", { size: 16 }));
      x.addEventListener("click", async () => {
        if (!(await confirm({ title: t("site.domainRemoveQ", { name: a }), body: t("site.domainRemoveBody", { name: a }), confirmLabel: t("site.domainRemoveBtn"), danger: true }))) return;
        save(aliases.filter((y) => y !== a));
      });
      rows.push(h("li", { class: "site-addr" }, icon("link", { size: 16 }), h("code", { class: "grow" }, a), x));
    }
    let www = null;
    if (apex) {
      www = toggle(t("site.wwwToo", { name: "www." + domain }), { checked: m.www !== false });
      www.input.addEventListener("change", () => save(aliases, www.input.checked));
    }
    const input = h("input", { type: "text", placeholder: t("sitepage.domainPlaceholder"), "aria-label": t("site.domainAdd"), autocomplete: "off", autocapitalize: "off", spellcheck: "false" });
    const add = h("button", { type: "submit", class: "btn" }, icon("plus"), t("site.domainAdd"));
    const form = h("form", { class: "site-inline-form", novalidate: true }, input, add);
    form.addEventListener("submit", (e) => {
      e.preventDefault();
      const v = input.value.trim().toLowerCase().replace(/^https?:\/\//, "").replace(/\/.*$/, "");
      if (!v || v === domain || aliases.includes(v)) return;
      save([...aliases, v]);
    });
    return h("section", { class: "card site-section" },
      h("h2", null, t("site.domains")), h("p", { class: "small muted" }, t("site.domainsHint")),
      h("ul", { class: "site-addr-list" }, ...rows),
      www ? h("div", { class: "site-www" }, www.el) : null,
      form);
  }

  // Forcing https, one switch per name the site answers to: a site with
  // several names may keep one on plain http (an old device, a webhook that
  // cannot follow a redirect). The house sends http:// of a marked name to
  // https:// before anything else answers it.
  function httpsCard(m) {
    const names = siteNames(m, domain);
    if (!names.length) return null;
    const forced = new Set((m.force_https || []).map((n) => String(n).toLowerCase()));
    const save = async (next) => {
      try {
        await api.patch(`/sites/${enc(domain)}/manifest`, { force_https: next.length ? next : null });
        toast(t("site.httpsSaved"), "ok");
      } catch (e) { toastError(e); }
    };
    const rows = names.map((name) => {
      const sw = toggle(h("code", null, name), { checked: forced.has(name) });
      sw.input.addEventListener("change", () => {
        // From the manifest as it is now, not as it was drawn: two
        // switches flipped quickly must both stick.
        const now = new Set(((store.site(domain).manifest || {}).force_https || []).map((n) => String(n).toLowerCase()));
        if (sw.input.checked) now.add(name); else now.delete(name);
        save(names.filter((n) => now.has(n)).concat([...now].filter((n) => !names.includes(n))));
      });
      return h("li", { class: "site-addr site-https" }, icon("lock", { size: 16 }), sw.el);
    });
    return h("section", { class: "card site-section" },
      h("h2", null, t("site.https")), h("p", { class: "small muted" }, t("site.httpsHint")),
      h("ul", { class: "site-addr-list" }, ...rows));
  }

  // Out of its container, onto the machine as a normal process: the same
  // command in the same folder (the container mounts it at the same path),
  // and the docker section kept for going back. A command that needs Node
  // gets one on the machine, installed if it has none.
  async function setRuntime(m, runtime) {
    const out = runtime === "process";
    if (!(await confirm({ title: t("site.uncontainQ", { domain }), body: t("site.uncontainBody"), confirmLabel: t("site.uncontain") }))) return;
    try {
      await api.patch(`/sites/${enc(domain)}/manifest`, { run: { ...m.run, runtime } });
      toast(t(out ? "site.uncontained" : "site.contained"), "ok");
    } catch (e) { toastError(e); }
  }

  // Into a container, or back into the one it had: the house works it out
  // and tries it beside the one that serves before anything is written;
  // when it cannot, the agent picked here is asked in a new conversation,
  // and the chat is where this goes next. Back into a kept container goes
  // the same way, tried first, so the button never leaves a site retrying.
  async function containSite() {
    let agents = [];
    try { agents = ((await api.get("/agents")).providers || []).filter((a) => a.available); } catch { agents = []; }
    const sel = h("select", null, h("option", { value: "" }, t("site.containNoAgent")), ...agents.map((a) => h("option", { value: a.provider }, a.provider)));
    if (agents.length) sel.value = agents[0].provider;
    const go = await open({
      title: t("site.containizeQ", { domain }),
      body: [h("p", { class: "dim" }, t("site.containizeBody")), field(t("site.containAgent"), sel)],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("site.containize"), primary: true, value: "go" }],
    });
    if (go !== "go") return;
    try {
      await api.post(`/sites/${enc(domain)}/containerize`, sel.value ? { provider: sel.value } : {});
    } catch (e) { toastError(e); return; }
    followContain("planning", "");
  }

  // The job's line lives outside what render() rebuilds: the site's state
  // changes while it is tried, and every change used to wipe the line
  // that said how it went. Opening the site again finds a job still at
  // work (GET containerize) and follows it from where it is.
  let containBox = null;
  let containOff = null;
  function followContain(step, message) {
    if (!containBox) { containBox = h("div"); el.prepend(containBox); }
    const say = (st, msg) => {
      containBox.textContent = t("site.containStep." + st) + (msg ? " — " + msg : "");
      containBox.className = "notice " + ({ planning: "info", trying: "info", done: "ok", asked: "warn" }[st] || "bad");
    };
    say(step, message);
    if (containOff) containOff();
    containOff = onEvent("containerize.", (ev) => {
      if (ev.subject !== domain) return;
      const st = ev.type.slice("containerize.".length);
      say(st, ev.message);
      if (st === "trying") return;
      containOff(); containOff = null;
      if (st === "done") toast(t("site.contained"), "ok");
      if (st === "asked") {
        containBox.append(" ", h("a", { href: `#/sites/${enc(domain)}/chat` }, t("site.containOpenChat")));
      }
    });
    subs.push(() => { if (containOff) containOff(); });
  }
  (async () => {
    if (!owner) return;
    try {
      const job = await api.get(`/sites/${enc(domain)}/containerize`);
      if (job && !job.done) followContain(job.step === "trying" ? "trying" : "planning", job.how || "");
    } catch { /* none since this node started */ }
  })();

  // Moving the site to another node: chosen, said (a short stop), and then
  // followed step by step from the events until it runs there.
  async function moveSite() {
    const sel = h("select", null, ...otherNodes.map((x) => h("option", { value: x.id }, x.name)));
    // What the move will do, read before it is confirmed (GET move/plan):
    // what is copied, where the target installs and with what, the Node,
    // the names, and what will not travel well. Asked again when the
    // target changes, since the target is asked whether it can take it.
    const planEl = h("div", { class: "move-plan" }, loading());
    let plan = null;
    async function readPlan() {
      plan = null;
      clear(planEl).append(loading());
      try { plan = await api.get(`/sites/${enc(domain)}/move/plan?node=${enc(sel.value)}`); } catch (e) { clear(planEl).append(notice("bad", e.message)); return; }
      clear(planEl).append(planView(plan));
    }
    sel.addEventListener("change", readPlan);
    readPlan();
    const node = await open({
      title: t("site.moveQ", { domain }),
      body: [h("p", { class: "dim" }, t("site.moveBody")), field(t("site.moveTo"), sel), planEl],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("site.move"), primary: true, value: "go" }],
    });
    if (node !== "go") return;
    if (plan && plan.check_error) { toast(plan.check_error, "bad"); return; }
    try {
      await api.post(`/sites/${enc(domain)}/move`, { node: sel.value });
    } catch (e) { toastError(e); return; }
    openMove(domain);
  }

  function planView(p) {
    const a = p.analysis || {};
    const items = [];
    items.push(h("li", null, t("plan.copy", { size: fmtBytes(a.bytes || 0), files: (a.files || 0).toLocaleString() })));
    if ((a.installs || []).length) {
      items.push(h("li", null, t("plan.installs"), h("ul", null, ...a.installs.map((i) => h("li", null, h("code", null, i.dir === "." ? t("plan.root") : i.dir + "/"), " — ", h("code", { class: "small" }, i.command.slice(0, 2).join(" ")), i.lockfile ? null : h("span", { class: "badge warn" }, t("plan.noLock")))))));
    } else {
      items.push(h("li", null, t("plan.noInstalls")));
    }
    const node = a.node || p.node_pinned;
    if (node) items.push(h("li", null, t(a.node ? "plan.node" : "plan.nodePinned", { v: node })));
    if (a.docker) items.push(h("li", null, t("plan.docker")));
    if ((a.names || []).length) items.push(h("li", null, t("plan.names", { names: a.names.join(", ") })));
    else items.push(h("li", null, t("plan.noNames")));
    const out = [h("h3", null, t("plan.title")), h("ul", { class: "plan-list" }, ...items)];
    for (const w of a.warnings || []) out.push(notice("warn", w));
    if ((a.outside_links || []).length) {
      out.push(h("details", { class: "small" }, h("summary", null, t("plan.links", { n: a.outside_links.length })),
        h("ul", null, ...a.outside_links.map((l) => h("li", null, h("code", null, l.path), " → ", h("code", null, l.target))))));
    }
    if (p.check_error) out.push(notice("bad", t("plan.cannot", { node: p.node || "", why: p.check_error })));
    else if (p.node) out.push(h("p", { class: "small level-ok" }, t("plan.can", { node: p.node })));
    return h("div", { class: "stack-sm" }, ...out);
  }

  // The site's redirects: an old address sent to a new one with a 3xx,
  // before the site is served. In order: the first that matches wins.
  function redirectsCard(m) {
    const rules = [...(m.redirects || [])];
    const save = async (next) => {
      try {
        await api.patch(`/sites/${enc(domain)}/manifest`, { redirects: next.length ? next : null });
        toast(t("site.redirectsSaved"), "ok");
      } catch (e) { toastError(e); }
    };
    // In order, since the first that matches wins: from, an arrow, to,
    // the code as a quiet badge and a × at the end.
    const list = h("ol", { class: "site-redirects" });
    rules.forEach((r, i) => {
      const x = h("button", { type: "button", class: "icon-btn sm", "aria-label": t("site.redirectRemove"), title: t("site.redirectRemove") }, icon("x", { size: 16 }));
      x.addEventListener("click", () => save(rules.filter((_, j) => j !== i)));
      list.append(h("li", { class: "site-redirect" },
        h("code", { class: "from" }, r.from), h("span", { class: "arrow", "aria-hidden": "true" }, "→"), h("code", { class: "to" }, r.to),
        h("span", { class: "badge", title: t("site.redirectCode." + (r.code || 301)) }, String(r.code || 301)), x));
    });
    const from = h("input", { type: "text", placeholder: "/viejo, /blog/*, " + domain + "/*", autocomplete: "off", spellcheck: "false" });
    const to = h("input", { type: "text", placeholder: "https://nuevo.example.com/*", autocomplete: "off", spellcheck: "false" });
    const code = h("select", null, ...[301, 302, 307, 308, 303].map((c) => h("option", { value: String(c) }, t("site.redirectCode." + c))));
    const add = h("button", { type: "submit", class: "btn" }, icon("plus"), t("site.redirectAdd"));
    // One line on a wide screen (from, to, the code, Add), stacked on a
    // phone with the code and Add sharing the last line.
    const form = h("form", { novalidate: true, class: "site-redirect-form" },
      field(t("site.redirectFrom"), from), field(t("site.redirectTo"), to), field(t("sitepage.redirectCode"), code),
      h("div", { class: "field site-redirect-add" }, add));
    form.addEventListener("submit", (e) => {
      e.preventDefault();
      const f = from.value.trim(), d = to.value.trim();
      if (!f || !d) return;
      const c = Number(code.value);
      save([...rules, c === 301 ? { from: f, to: d } : { from: f, to: d, code: c }]);
    });
    return h("section", { class: "card site-section" },
      h("h2", null, t("site.redirects")), h("p", { class: "small muted" }, t("site.redirectsHint")),
      rules.length ? list : h("p", { class: "site-none" }, t("sitepage.redirectsNone")),
      form);
  }

  // A name the zone already points somewhere else: said, and taken only
  // with an explicit yes, because the record may be somebody's shop or mail.
  function conflictEl(name, r) {
    const take = h("button", { type: "button", class: "btn sm danger" }, t("publish.takeover"));
    take.addEventListener("click", async () => {
      if (!(await confirm({ title: t("publish.takeoverQ", { name }), body: t("publish.takeoverBody", { name, found: r.found, site: domain }), confirmLabel: t("publish.takeover"), danger: true }))) return;
      try {
        await api.post("/publish/takeover", { names: [name] });
        toast(t("publish.takenOver", { name }), "ok");
        loadPublished();
      } catch (e) { toastError(e); }
    });
    return h("div", { class: "small" }, h("span", { class: "dim" }, t("publish.conflictSays", { found: r.found })), " ", take);
  }

  function render() {
    const s = store.site(domain);
    clear(el);
    if (containBox) el.append(containBox);
    if (!s) { el.append(empty(t("site.gone"), t("site.goneHint"))); return; }
    const mode = siteMode(s);
    const m = s.manifest || {};
    const unmanaged = mode === "unmanaged";

    const held = Boolean(m.hold && m.hold.enabled);
    const project = mode === "project";
    const serves = !unmanaged && !project;

    // Actions, in two places. In sight: what this state asks for, one of
    // them the primary (start a stopped site, release a held one, publish
    // one nobody can reach yet) and at most two plain ones beside it.
    // Under "…": the rest, which are used a few times in a site's life,
    // and Delete last, set apart and red. Ten equal buttons in a row made
    // "Start" read like "Download". Each verb still confirms what
    // destroys, with the domain in the question.
    const stopIt = async () => {
      if (await confirm({ title: t("site.stopQ", { domain }), body: t("site.stopBody"), confirmLabel: t("site.stop"), danger: true })) {
        await act("stop", `/sites/${enc(domain)}/stop`, t("site.stopped"));
      }
    };
    const startIt = () => act("start", `/sites/${enc(domain)}/start`, t("site.started"));
    const restartIt = () => act("restart", `/sites/${enc(domain)}/restart`, t("site.restarted"));
    const shown = [];
    const folded = [];
    if (!owner) {
      delegateActions(m, { mode, held, project, stopIt, startIt, restartIt }, shown, folded);
    } else if (unmanaged) {
      shown.push(btn(t("site.adopt"), "plus", adopt, "primary"));
    } else {
      // Publish is the primary only once the names are known to be none:
      // before /publish answers it would flash blue on every site.
      const publishShown = serves && publishedLoaded && !published.length && !m.disabled && !held;
      if (m.disabled) shown.push(btn(t("site.start"), "play", startIt, "primary"));
      else if (held) shown.push(btn(t("site.release"), "unlock", () => hold(false), "primary"));
      else if (project) shown.push(btn(t("site.giveDomain"), "globe", rename, "primary"));
      else if (publishShown) shown.push(btn(t("site.publish"), "upload", publish, "primary"));
      if (!m.disabled && !held && (mode === "run" || mode === "services" || mode === "upstream")) shown.push(btn(t("site.restart"), "refresh", restartIt));
      if (!m.disabled && !project) shown.push(btn(t("site.stop"), "stop", stopIt));
      if (m.deploy && m.deploy.github) shown.push(btn(t("site.deploy"), "upload", () => act("deploy", `/sites/${enc(domain)}/deploy`, t("site.deployStarted"))));

      if (!m.disabled && project) folded.push({ label: t("site.stop"), icon: "stop", onClick: stopIt });
      if (m.disabled && (mode === "run" || mode === "services" || mode === "upstream")) folded.push({ label: t("site.restart"), icon: "refresh", onClick: restartIt });
      if (held && !m.disabled) folded.push({ label: t("site.stop"), icon: "stop", onClick: stopIt });
      if (held && m.disabled) folded.push({ label: t("site.release"), icon: "unlock", onClick: () => hold(false) });
      if (!held) folded.push({ label: t("site.hold"), icon: "pause", onClick: () => hold(true) });
      if (serves) {
        if (!publishShown) folded.push({ label: t("site.publish"), icon: "upload", onClick: publish });
        if (published.length) folded.push({ label: t("site.unpublish"), icon: "door", onClick: unpublish });
      }
      folded.push({ separator: true });
      // A plain link: the browser downloads with the session cookie, and a
      // site of gigabytes streams instead of passing through memory.
      folded.push({ label: t("site.download"), icon: "download", href: apiURL(`/sites/${enc(domain)}/download.zip`), download: `${domain}.zip`, title: t("site.downloadHint") });
      if (!project) folded.push({ label: t("site.rename"), icon: "edit", onClick: rename });
      folded.push({ label: t("site.exec"), icon: "terminal", onClick: runCommand });
      if (otherNodes.length) folded.push({ label: t("site.move"), icon: "upload", onClick: moveSite });
      // In a container, or out of one with its container kept to go back to.
      if (m.run && m.run.runtime === "docker") folded.push({ label: t("site.uncontain"), icon: "box", onClick: () => setRuntime(m, "process") });
      else if (mode === "run") folded.push({ label: t(m.run && m.run.docker ? "site.contain" : "site.containize"), icon: "box", onClick: containSite });
    }
    if (owner) folded.push({ separator: true }, { label: t("site.delete"), icon: "trash", onClick: del, danger: true });
    const actions = h("div", { class: "site-actions" }, ...shown, menu(t("sitepage.more"), folded, { class: "site-actions-more" }));

    // What there is to know about it, as short facts: a label over its
    // value, in as many columns as fit. Facts the site does not have are
    // left out rather than shown as a dash.
    const facts = [
      [t("site.port"), s.port ? h("code", null, String(s.port)) : null],
      [t("site.revision"), s.revision ? h("code", { title: s.revision }, shortRev(s.revision)) : null],
      [t("site.runtime"), m.run && m.run.runtime ? m.run.runtime : null],
      [t("site.upstream"), m.upstream ? h("code", null, m.upstream) : null],
      [t("site.staticRoot"), m.static ? h("code", null, (m.static.root || "public") + "/") : null],
      [t("site.holdTitle"), held ? (m.hold.title || t("sitepage.holdNoTitle")) : null],
      [t("site.handing"), s.handing ? t("common.yes") : null],
    ].filter(([, v]) => v);

    // Names the house published for it, each with how that went: the
    // operator's DNS, not shown to a delegated user.
    let names = null;
    if (serves && owner) {
      names = h("div", { class: "site-names" }, h("div", { class: "site-label" }, t("site.published")),
        !publishedLoaded ? h("span", { class: "muted small" }, t("common.loading"))
          : published.length ? h("div", { class: "site-name-list" }, ...published.map(([name, r]) => h("div", { class: "site-name-row" },
            h("code", null, name),
            h("span", { class: "badge " + ({ published: "ok", failed: "bad", manual: "warn", retiring: "warn", conflict: "bad" }[r.state] || ""), title: r.error || "" }, t("publish.state." + r.state)),
            h("span", { class: "small muted" }, t("inbound.door." + r.door)),
            r.state === "conflict" ? conflictEl(name, r) : r.error ? h("div", { class: "small dim site-name-err" }, r.error) : null,
          )))
            : h("span", { class: "small dim" }, t("sitepage.notPublishedHint")));
    }

    const problems = [
      s.error ? notice("bad", h("strong", null, t("site.error")), " ", s.error) : null,
      (s.missing || []).length ? notice("warn", h("strong", null, t("site.missing")), h("ul", { class: "site-missing" }, ...s.missing.map((x) => h("li", null, x)))) : null,
    ];

    const hero = h("section", { class: "card site-hero" + (serves ? "" : " no-shot") },
      serves ? sitePicture() : null,
      h("div", { class: "site-hero-main" },
        h("div", { class: "site-hero-state" }, h("span", { class: "site-state-big" }, aliveStatus(s)), ["hold", "stopped", "unmanaged"].includes(mode) ? null : modeBadge(s)),
        ...problems,
        facts.length ? h("dl", { class: "site-facts" }, ...facts.map(([k, v]) => h("div", null, h("dt", null, k), h("dd", null, v)))) : null,
        names,
        actions));

    append(el, [
      hero,
      unmanaged ? notice("warn", t("site.unmanagedNotice")) : null,
      servicesCard(s),
      workersCard(s),
      serves && owner && !Object.keys(m.services || {}).length ? domainsCard(m) : null,
      serves && can(domain, "code") ? redirectsCard(m) : null,
      serves && can(domain, "code") ? httpsCard(m) : null,
      unmanaged || !door ? null : door,
      unmanaged ? null : envCard,
    ]);
  }

  // A site with services: one line each, as a worker's, with its names
  // and what answers them, and below, every name of the site and where it
  // goes now (GET /routes), which is what to look at when a name answers
  // the house's page.
  function servicesCard(s) {
    const m = s.manifest || {};
    const names = Object.keys(m.services || {}).sort();
    if (names.length === 0) return null;
    const items = names.map((name) => {
      const v = (s.services && s.services[name]) || {};
      const def = m.services[name] || {};
      const run = def.run || {};
      let state;
      if (v.stopped === "disabled") state = h("span", { class: "status" }, h("span", { class: "dot" }), t("status.stopped"));
      else if (v.stopped === "hold") state = h("span", { class: "status" }, h("span", { class: "dot warn" }), t("site.serviceHeld"));
      else if (v.alive) state = h("span", { class: "status" }, h("span", { class: "dot ok" }), t("status.up"));
      else if (v.starting || v.waiting) state = h("span", { class: "status", title: v.waiting ? t("site.serviceWaiting", { names: v.waiting }) : "" }, h("span", { class: "dot warn" }), t("site.serviceStarting"));
      else state = h("span", { class: "status", title: v.error || "" }, h("span", { class: "dot bad" }), t("status.down"));
      const what = run.runtime === "docker" && run.docker ? (run.docker.image || t("site.serviceBuilt", { file: run.docker.build || "" })) : (run.command || []).join(" ");
      return h("li", { class: "site-worker" },
        h("div", { class: "site-worker-main" },
          h("div", { class: "site-worker-head" }, h("strong", null, name), state,
            v.port ? h("code", { class: "small muted", title: v.published ? "" : t("site.serviceInside") }, ":" + v.port) : null,
            v.revision ? h("code", { class: "small dim" }, shortRev(v.revision)) : null,
            def.optional ? h("span", { class: "badge" }, t("site.serviceOptional")) : null),
          (def.domains || []).length ? h("div", { class: "small" }, ...def.domains.map((d) => h("code", { class: "site-service-name" }, d)))
            : h("div", { class: "small muted" }, t("site.serviceInternal")),
          what ? h("code", { class: "site-worker-cmd", title: what }, what) : null,
          v.waiting && !v.alive ? h("div", { class: "small muted" }, t("site.serviceWaiting", { names: v.waiting })) : null,
          v.error ? h("div", { class: "small level-error" }, v.error) : null),
        serviceActions(name, def, v));
    });
    const routes = h("div", { class: "site-routes" }, h("span", { class: "small muted" }, t("common.loading")));
    api.get(`/sites/${enc(domain)}/routes`).then((rs) => {
      const cols = [t("site.routeName"), t("site.routeService"), t("site.routePort"), t("site.routeState")];
      clear(routes).append(table(cols.map((label) => ({ label })),
        (rs || []).map((r) => h("tr", null,
          cell(h("code", null, r.host + (r.prefix && r.prefix !== "/" ? r.prefix : "")), cols[0]),
          cell(r.service || "—", cols[1]),
          cell(r.port ? String(r.port) : "—", cols[2]),
          cell(h("span", { title: r.error || "" }, r.state), cols[3]),
        )), { responsive: true }).el);
    }).catch((e) => clear(routes).append(errorBox(e)));
    return h("section", { class: "card site-section" },
      h("h2", null, t("site.services")),
      h("ul", { class: "site-workers" }, ...items),
      h("h3", { class: "small" }, t("site.routes")),
      routes);
  }

  // Restart is for now; stop, start and hold are written to the
  // service's section of the manifest, so they survive a restart.
  function serviceActions(name, def, v) {
    const tabs = `#/sites/${enc(domain)}`;
    const run = def.run || {};
    const box = h("div", { class: "site-worker-actions" },
      h("a", { class: "btn sm ghost", href: `${tabs}/logs?service=${enc(name)}` }, icon("list"), t("site.workerLogs")),
      run.runtime === "docker" && run.docker && run.docker.build
        ? h("a", { class: "btn sm ghost", href: `${tabs}/logs?service=${enc(name)}&build=1` }, icon("list"), t("site.serviceBuildLog")) : null);
    if (!can(domain, "operate")) return box;
    const path = `/sites/${enc(domain)}/services/${enc(name)}`;
    if (def.disabled) {
      box.append(btn(t("site.workerStart"), "play", () => act("start", path + "/start", t("site.workerStarted", { name })), "sm"));
      return box;
    }
    if (def.hold && def.hold.enabled) {
      box.append(btn(t("site.release"), "unlock", () => holdService(path, false, name), "sm"));
      return box;
    }
    if (v.alive || v.error) box.append(btn(t("site.restart"), "refresh", () => act("restart", path + "/restart", t("site.workerRestarted", { name })), "sm"));
    box.append(btn(t("site.hold"), "pause", () => holdService(path, true, name), "sm"));
    box.append(btn(t("site.stop"), "stop", async () => {
      if (await confirm({ title: t("site.serviceStopQ", { name }), body: t("site.serviceStopBody"), confirmLabel: t("site.stop"), danger: true })) {
        await act("stop", path + "/stop", t("site.workerStopped", { name }));
      }
    }, "sm"));
    return box;
  }

  async function holdService(path, on, name) {
    const r = await api.post(path + "/hold", { enabled: on });
    if (r && r.domain) store.putSite(r);
    toast(on ? t("site.serviceHeldDone", { name }) : t("site.serviceReleased", { name }), "ok");
  }

  function workersCard(s) {
    const m = s.manifest || {};
    const names = Object.keys(m.workers || {});
    if (names.length === 0) return null;
    const usage = {};
    // A worker a line: its state and name, its command quiet under them,
    // what it takes now, and its buttons at the end. A table squeezed the
    // command into two lines and the buttons into two rows.
    const items = names.map((name) => {
      const w = (s.workers && s.workers[name]) || {};
      const def = m.workers[name] || {};
      const port = def.port || w.port;
      const state = def.disabled ? h("span", { class: "status" }, h("span", { class: "dot" }), t("status.stopped"))
        : w.alive ? h("span", { class: "status" }, h("span", { class: "dot ok" }), t("status.up"))
          : h("span", { class: "status", title: w.error || "" }, h("span", { class: "dot bad" }), t("status.down"));
      return h("li", { class: "site-worker" },
        h("div", { class: "site-worker-main" },
          h("div", { class: "site-worker-head" }, h("strong", null, name), state, port ? h("code", { class: "small muted" }, ":" + port) : null,
            usage[name] = h("span", { class: "small muted site-worker-usage" })),
          def.command ? h("code", { class: "site-worker-cmd", title: def.command.join(" ") }, def.command.join(" ")) : null,
          w.error ? h("div", { class: "small level-error" }, w.error) : null),
        workerActions(name, def, w));
    });
    // What each takes now, from the site's last sample (one request for
    // all of them, after the card is drawn).
    api.get(`/sites/${enc(domain)}/resources`).then((r) => {
      for (const name of names) {
        const x = (r.workers || {})[name];
        clear(usage[name]).append(x ? t("site.usageNow", { cpu: x.cpu, mem: fmtBytes(x.rss) }) : "");
      }
    }).catch(() => { for (const name of names) clear(usage[name]); });
    return h("section", { class: "card site-section" }, h("h2", null, t("site.workers")), h("ul", { class: "site-workers" }, ...items));
  }

  // Restart is for now; stop and start are written to the manifest
  // (workers.<name>.disabled), so they survive a restart of the house.
  function workerActions(name, def, w) {
    // The site's own address, here: this tab is not inside siteScreen,
    // whose base the first version of these links used and broke the
    // overview of every site with a worker.
    const tabs = `#/sites/${enc(domain)}`;
    const box = h("div", { class: "site-worker-actions" },
      h("a", { class: "btn sm ghost", href: `${tabs}/logs?worker=${enc(name)}` }, icon("list"), t("site.workerLogs")),
      h("a", { class: "btn sm ghost", href: `${tabs}/activity?worker=${enc(name)}` }, icon("gauge"), t("site.workerUsage")));
    const path = `/sites/${enc(domain)}/workers/${enc(name)}`;
    if (!can(domain, "operate")) return box;
    if (def.disabled) {
      box.append(btn(t("site.workerStart"), "play", () => act("start", path + "/start", t("site.workerStarted", { name })), "sm"));
      return box;
    }
    if (w.alive) box.append(btn(t("site.restart"), "refresh", () => act("restart", path + "/restart", t("site.workerRestarted", { name })), "sm"));
    box.append(btn(t("site.stop"), "stop", async () => {
      if (await confirm({ title: t("site.workerStopQ", { name }), body: t("site.workerStopBody"), confirmLabel: t("site.stop"), danger: true })) {
        await act("stop", path + "/stop", t("site.workerStopped", { name }));
      }
    }, "sm"));
    return box;
  }

  // What a delegated user's grant lets them do from here: the state
  // with operate, the deploy and a command with code, the download with
  // any. Publishing, renaming, moving, containers and deleting are the
  // operator's.
  function delegateActions(m, st, shown, folded) {
    const { mode, held, project, stopIt, startIt, restartIt } = st;
    if (can(domain, "operate")) {
      if (m.disabled) shown.push(btn(t("site.start"), "play", startIt, "primary"));
      else if (held) shown.push(btn(t("site.release"), "unlock", () => hold(false), "primary"));
      if (!m.disabled && !held && (mode === "run" || mode === "services" || mode === "upstream")) shown.push(btn(t("site.restart"), "refresh", restartIt));
      if (!m.disabled && !project) shown.push(btn(t("site.stop"), "stop", stopIt));
      if (!held) folded.push({ label: t("site.hold"), icon: "pause", onClick: () => hold(true) });
    }
    if (can(domain, "code") && m.deploy && m.deploy.github) shown.push(btn(t("site.deploy"), "upload", () => act("deploy", `/sites/${enc(domain)}/deploy`, t("site.deployStarted"))));
    folded.push({ separator: true });
    folded.push({ label: t("site.download"), icon: "download", href: apiURL(`/sites/${enc(domain)}/download.zip`), download: `${domain}.zip`, title: t("site.downloadHint") });
    if (can(domain, "code")) folded.push({ label: t("site.exec"), icon: "terminal", onClick: runCommand });
  }

  // One command where the site runs, answered in the same dialog. Not a
  // terminal: no input, a minute by default; anything longer is a task.
  async function runCommand() {
    const cmd = h("input", { type: "text", class: "mono", placeholder: "npm ls --depth=0", autocomplete: "off", spellcheck: "false" });
    const timeout = h("select", null, ...["30s", "1m", "5m", "10m"].map((d) => h("option", { value: d }, d)));
    timeout.value = "1m";
    const out = h("pre", { class: "log exec-out", hidden: true });
    const status = h("p", { class: "small" });
    const svcNames = Object.keys((store.site(domain).manifest || {}).services || {}).sort();
    const service = h("select", null, ...svcNames.map((n) => h("option", { value: n }, n)));
    await open({
      title: t("site.execQ", { domain }),
      wide: true,
      body: [h("p", { class: "dim small" }, t("site.execBody")), h("div", { class: "form-row" }, svcNames.length ? field(t("site.execService"), service) : null, field(t("site.execCommand"), cmd), field(t("site.execTimeout"), timeout)), status, out],
      buttons: [{ label: t("common.close"), value: null }, { label: t("site.execRun"), submit: true, primary: true }],
      onSubmit: async () => {
        if (!cmd.value.trim()) return false;
        status.textContent = t("site.execRunning");
        out.hidden = true;
        const r = await api.post(`/sites/${enc(domain)}/exec`, { command: cmd.value, timeout: timeout.value, service: svcNames.length ? service.value : "" });
        out.textContent = r.output || t("site.execNoOutput");
        out.hidden = false;
        status.textContent = (r.timed_out ? t("site.execTimedOut") : t("site.execExit", { code: r.exit_code })) +
          " · " + r.seconds.toFixed(1) + " s" + (r.truncated ? " · " + t("site.execTruncated") : "");
        status.className = "small " + (r.exit_code === 0 && !r.timed_out ? "level-ok" : "level-error");
        return false; // stay open: the answer is the point
      },
    });
  }

  async function loadEnv() {
    try {
      const r = await api.get(`/sites/${enc(domain)}/env`);
      const resolved = (r && r.resolved) || {};
      const keys = Object.keys(resolved).sort();
      clear(envBox);
      envCount.textContent = keys.length ? String(keys.length) : t("sitepage.envNone");
      envCount.className = keys.length ? "badge" : "small muted";
      if (keys.length === 0) { envBox.append(h("p", { class: "site-none" }, t("site.envEmpty"))); return; }
      // Name over value, wrapping anywhere: a table cut the paths in the
      // middle on a phone and scrolled the card sideways.
      envBox.append(h("dl", { class: "site-env-list" }, ...keys.map((k) => h("div", null, h("dt", null, k), h("dd", null, String(resolved[k]))))));
    } catch (e) {
      envCount.textContent = "";
      clear(envBox).append(errorBox(e));
    }
  }

  function btn(label, ico, fn, cls) {
    const b = h("button", { type: "button", class: "btn " + (cls || "") }, icon(ico), label);
    b.addEventListener("click", async () => {
      b.classList.add("busy");
      try { await fn(); } catch (e) { toastError(e); } finally { b.classList.remove("busy"); }
    });
    return b;
  }

  async function act(_what, path, done) {
    const r = await api.post(path);
    if (r && r.domain) store.putSite(r);
    toast(done, "ok");
  }

  async function hold(enabled) {
    const s = store.site(domain);
    const cur = (s.manifest && s.manifest.hold) || {};
    if (!enabled) {
      const r = await api.post(`/sites/${enc(domain)}/hold`, { enabled: false });
      if (r && r.domain) store.putSite(r);
      toast(t("site.released"), "ok");
      return;
    }
    const title = h("input", { type: "text", value: cur.title || "" });
    const message = h("textarea", { value: cur.message || "" });
    const mode = h("select", null, ...["maintenance", "404", "410", "500"].map((k) => h("option", { value: k }, t("site.holdMode." + k))));
    mode.value = cur.mode || "maintenance";
    // A local time in the picker, sent as RFC 3339: the page shows it and
    // it goes out as Retry-After.
    const until = h("input", { type: "datetime-local", value: cur.until ? new Date(cur.until).toISOString().slice(0, 16) : "" });
    const ok = await open({
      title: t("site.holdQ", { domain }),
      body: [h("p", { class: "dim" }, t("site.holdBody")), field(t("site.holdModeLabel"), mode), field(t("site.holdTitle"), title), field(t("site.holdMessage"), message), field(t("site.holdUntil"), until, t("site.holdUntilHint"))],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("site.hold"), submit: true, primary: true }],
      onSubmit: async () => {
        const body = { enabled: true, title: title.value, message: message.value, mode: mode.value === "maintenance" ? "" : mode.value };
        if (until.value) body.until = new Date(until.value).toISOString().replace(/\.\d{3}Z$/, "Z");
        const r = await api.post(`/sites/${enc(domain)}/hold`, body);
        if (r && r.domain) store.putSite(r);
      },
    });
    if (ok) toast(t("site.heldNow"), "ok");
  }

  // The site as a visitor sees it, taken through a door that skips its
  // password. With no picture yet the frame says so and offers to take
  // one, instead of a blank row with a lone "take again" in it; on a
  // machine with no browser to take it, the frame says that and the
  // button goes, for the rest of the visit (shotsOff).
  function sitePicture() {
    const frame = h("div", { class: "site-shot-frame" });
    const img = h("img", { class: "site-shot", alt: t("site.pictureAlt", { domain }), src: apiURL(`/sites/${enc(domain)}/shot.png?t=${Math.floor(Date.now() / 60000)}`), loading: "lazy" });
    const again = h("button", { type: "button", class: "btn sm ghost site-shot-again" }, icon("refresh", { size: 14 }), t("site.pictureAgain"));
    const placeholder = h("div", { class: "site-shot-none" });
    const take = async () => {
      try { await api.post(`/sites/${enc(domain)}/shot`); toast(t("site.pictureTaking"), "ok"); }
      catch (e) {
        if (e.status === 501) { shotsOff = true; paintNone(); return; }
        toastError(e);
      }
    };
    function paintNone() {
      const b = h("button", { type: "button", class: "btn sm" }, icon("eye", { size: 14 }), t("sitepage.pictureTake"));
      b.addEventListener("click", () => busy(b, take)());
      clear(placeholder).append(icon("monitor", { size: 28 }),
        h("strong", null, t(shotsOff ? "sitepage.pictureOff" : "sitepage.pictureNone")),
        h("span", { class: "small muted" }, t(shotsOff ? "sitepage.pictureOffHint" : "sitepage.pictureNoneHint")),
        ...(shotsOff ? [] : [b]));
    }
    again.addEventListener("click", () => busy(again, take)());
    img.addEventListener("error", () => { frame.classList.add("unshot"); paintNone(); });
    img.addEventListener("load", () => frame.classList.remove("unshot"));
    frame.append(img, placeholder, again);
    return frame;
  }
  subs.push(onEvent("shot.taken", (ev) => {
    if (ev.subject !== domain) return;
    const img = el.querySelector("img.site-shot");
    if (img) img.src = apiURL(`/sites/${enc(domain)}/shot.png?t=${Date.now()}`);
  }));

  async function publish() {
    try {
      const r = await api.post(`/sites/${enc(domain)}/publish`, {});
      const failed = (r.lines || []).filter((l) => l.status === "fail");
      toast(failed.length ? failed.map((l) => l.text).join("; ") : t("site.publishedNow"), failed.length ? "bad" : "ok");
      // Published with known critical vulnerabilities: said at the moment
      // the site starts being reachable.
      if (r.vulnerable) toast(r.vulnerable, "bad");
    } catch (e) { toastError(e); }
    loadPublished();
  }

  async function unpublish() {
    if (!(await confirm({ title: t("site.unpublishQ", { domain }), body: t("site.unpublishBody"), confirmLabel: t("site.unpublish"), danger: true }))) return;
    try {
      await api.post(`/sites/${enc(domain)}/unpublish`, {});
      toast(t("site.unpublishedNow"), "ok");
    } catch (e) { toastError(e); }
    loadPublished();
  }

  async function rename() {
    // A project (no dot) is given a domain the same way, and published on
    // it unless the box is cleared.
    const project = !domain.includes(".");
    const to = h("input", { type: "text", value: project ? "" : domain, placeholder: project ? "example.com" : "", autocomplete: "off", autocapitalize: "off", spellcheck: "false" });
    const pub = checkbox(t("site.renamePublish"), { checked: true });
    const made = await open({
      title: t(project ? "site.giveDomainQ" : "site.renameQ", { domain }),
      body: [h("p", { class: "dim" }, t(project ? "site.giveDomainBody" : "site.renameBody")), field(t(project ? "site.giveDomainTo" : "site.renameTo"), to), project ? pub.el : null],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t(project ? "site.giveDomain" : "site.rename"), submit: true, primary: true }],
      onSubmit: async () => {
        const r = await api.post(`/sites/${enc(domain)}/rename`, { to: to.value.trim().toLowerCase(), publish: project && pub.input.checked });
        store.dropSite(domain);
        if (r && r.domain) store.putSite(r);
        return r.domain;
      },
    });
    if (made) {
      toast(t("site.renamed", { domain: made }), "ok");
      navigate(`/sites/${encodeURIComponent(made)}`);
    }
  }

  async function del() {
    const archive = checkbox(t("site.archive"), { checked: true });
    const yes = await confirm({
      title: t("site.deleteQ", { domain }),
      body: t("site.deleteBody"),
      confirmLabel: t("site.delete"),
      danger: true,
      extra: h("div", { class: "field" }, archive.el, h("div", { class: "hint" }, t("site.archiveHint"))),
    });
    if (!yes) return;
    try {
      await api.del(`/sites/${enc(domain)}` + (archive.input.checked ? "?archive=true" : ""));
      store.dropSite(domain);
      toast(archive.input.checked ? t("site.archived", { domain }) : t("site.deleted", { domain }), "ok");
      navigate("/sites");
    } catch (e) { toastError(e); }
  }

  async function adopt() {
    const template = h("select", null, ...["static", "node", "go", "docker"].map((k) => h("option", { value: k }, t("template." + k))));
    const made = await open({
      title: t("site.adoptQ", { domain }),
      body: [h("p", { class: "dim" }, t("site.adoptBody")), field(t("sites.template"), template)],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("site.adopt"), submit: true, primary: true }],
      onSubmit: async () => {
        const site = await api.post("/sites", { domain, template: template.value });
        store.putSite(site);
      },
    });
    if (made) toast(t("site.adopted", { domain }), "ok");
  }

  subs.push(store.subscribe("site:" + domain, render));
  render();
  if (siteMode(store.site(domain) || {}) !== "unmanaged") { loadEnv(); loadPublished(); }
  return { el, destroy() { subs.forEach((u) => u()); } };
}

// ---------- manifest ----------

function manifestTab(domain) {
  const s = store.site(domain);
  const el = h("div", { class: "card" });
  const editor = jsonEditor({
    value: s.manifest || { version: 2 },
    onSave: async (value) => {
      const updated = await api.put(`/sites/${enc(domain)}/manifest`, value);
      if (updated && updated.domain) {
        loaded = JSON.stringify(updated.manifest || {}); // our own save is not a change underneath
        store.putSite(updated);
      }
      toast(t("manifest.saved"), "ok");
    },
  });
  const note = h("p", { class: "small muted" }, t("manifest.hint"));
  const stamp = h("p", { class: "small muted" });
  el.append(h("h2", null, t("site.tab.manifest")), note, editor.el, stamp);
  // A manifest edited by an agent or by hand while this tab is open shows
  // a note instead of silently replacing what the operator is typing. The
  // site object refreshes on every instance event, so only a manifest
  // that really differs from the one loaded counts as changed.
  let loaded = JSON.stringify(s.manifest || {});
  const unsub = store.subscribe("site:" + domain, () => {
    const site = store.site(domain);
    if (!site) return;
    const now = JSON.stringify(site.manifest || {});
    if (now === loaded) return;
    loaded = now;
    if (!editor.dirty) { editor.setValue(site.manifest || {}); stamp.textContent = ""; return; }
    stamp.textContent = t("manifest.changedUnderneath", { at: fmtTime(new Date().toISOString()) });
  });
  el.append(historyCard(domain, editor));
  return { el, destroy: unsub };
}

// The versions the engine kept, whoever wrote them. Looking at one puts
// nothing in the editor; restoring writes it back through the same
// validation as a save, and becomes the newest version itself.
function historyCard(domain, editor) {
  const box = h("div");
  const wrap = h("div", null, h("hr"), h("h3", null, t("manifest.history")), h("p", { class: "small muted" }, t("manifest.historyHint")), box);
  async function load() {
    try {
      const versions = await api.get(`/sites/${enc(domain)}/manifest/history`);
      clear(box);
      if (!versions.length) { box.append(h("p", { class: "small muted" }, t("manifest.historyNone"))); return; }
      box.append(table([{ label: t("manifest.historyAt") }, { label: t("manifest.historySize"), class: "num" }, { label: "" }],
        versions.map((v, i) => h("tr", null,
          cell(h("span", { title: fmtTime(v.at) }, fmtTime(v.at), " ", h("span", { class: "muted small" }, fmtAgo(v.at, t))), t("manifest.historyAt")),
          cell(fmtBytes(v.bytes), t("manifest.historySize"), "num"),
          cell(i === 0 ? h("span", { class: "badge ok" }, t("manifest.historyCurrent")) : versionActions(v), null, "right nowrap"),
        )), { responsive: true }).el);
    } catch (e) { clear(box).append(errorBox(e)); }
  }
  function versionActions(v) {
    const view = h("button", { type: "button", class: "btn sm" }, icon("eye"), t("manifest.historyView"));
    view.addEventListener("click", async () => {
      try {
        const text = await api.text(`/sites/${enc(domain)}/manifest/history/${enc(v.id)}`);
        const restore = await open({
          title: t("manifest.historyOf", { at: fmtTime(v.at) }), wide: true,
          body: h("pre", { class: "log" }, text),
          buttons: [{ label: t("common.close"), value: false }, { label: t("manifest.historyRestore"), value: true, primary: true }],
        });
        if (restore) await doRestore(v);
      } catch (e) { toastError(e); }
    });
    const back = h("button", { type: "button", class: "btn sm" }, icon("history"), t("manifest.historyRestore"));
    back.addEventListener("click", () => doRestore(v));
    return h("div", { class: "actions" }, view, back);
  }
  async function doRestore(v) {
    if (!(await confirm({ title: t("manifest.historyRestoreQ", { at: fmtTime(v.at) }), body: t("manifest.historyRestoreBody"), confirmLabel: t("manifest.historyRestore") }))) return;
    try {
      const updated = await api.post(`/sites/${enc(domain)}/manifest/history/${enc(v.id)}/restore`);
      if (updated && updated.domain) { store.putSite(updated); editor.setValue(updated.manifest || {}); }
      toast(t("manifest.historyRestored"), "ok");
      setTimeout(load, 3500); // the engine keeps it on its next tick
    } catch (e) { toastError(e); }
  }
  load();
  return wrap;
}

// Every name a site answers to, as the house's Hosts counts them: its
// own, its aliases, its services' domains (without their paths), and the
// www. of each apex unless the manifest says www: false.
function siteNames(m, domain) {
  const out = [];
  const add = (n) => {
    n = String(n || "").trim().toLowerCase().replace(/\.$/, "").split("/")[0];
    if (n && !out.includes(n)) out.push(n);
  };
  const extra = [...(m.aliases || [])];
  for (const sv of Object.values(m.services || {})) extra.push(...((sv && sv.domains) || []));
  add(domain);
  extra.forEach(add);
  if (m.www !== false) {
    for (const n of [...out]) if (n.split(".").length === 2 && !n.startsWith("www.")) add("www." + n);
  }
  return out;
}

// The names a site with services answers, each with its service, the
// site's own first: an internal service has none, and a name with a path
// ("bakery.test/api") opens at that path.
function openableNames(s, domain) {
  const m = s.manifest || {};
  const svcs = m.services || {};
  const out = [];
  const seen = new Set();
  for (const service of Object.keys(svcs).sort()) {
    for (const d of svcs[service].domains || []) {
      const name = String(d).replace(/\/+$/, "");
      if (!name || seen.has(name)) continue;
      seen.add(name);
      out.push({ name, service });
    }
  }
  out.sort((a, b) => (b.name === domain) - (a.name === domain) || a.name.localeCompare(b.name));
  return out;
}
