// Home: every site on the machine, live. Rows are re-rendered from the
// store whenever the stream touches a site, so a handover shows as it
// happens; the search box filters in memory because the list is already
// here and a round trip would only add a delay.

import { h, clear, shortRev, fmtAgo, fmtTime, fmtBytes, debounce } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, apiURL } from "../lib/api.js";
import { store } from "../lib/store.js";
import { navigate } from "../lib/router.js";
import { page, empty, errorBox, loading, table, cell, notice, search as searchBox } from "../components/page.js";
import { icon } from "../lib/icons.js";

import { modeBadge, aliveStatus, siteMode } from "../components/badge.js";
import { open, confirm, field, checkbox } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { currentNode, setCurrentNode } from "../lib/node.js";
import { delegate, can } from "../lib/access.js";
import { menu } from "./site-menu.js";
import { randomLabel, withLabel, commonParent, MAX_WORDS } from "../lib/words.js";

export function sitesScreen({ query }) {
  const search = h("input", { type: "search", placeholder: t("sites.search"), "aria-label": t("sites.search"), autocomplete: "off" });
  const newBtn = h("button", { type: "button", class: "btn primary", "data-shortcut": "new" }, icon("plus"), t("sites.new"));
  // Archived sites and importing one are used a few times a year: they
  // fold under "…" so that, on a phone, the search and New site keep the
  // header to themselves.
  const more = menu(t("sitelist.more"), [
    { label: t("sites.archived"), icon: "history", onClick: () => showArchived() },
    { label: t("sites.importV1"), icon: "download", onClick: () => importV1() },
  ]);
  // A delegated user manages the sites they were given and makes none.
  const mine = delegate();
  const p = page({ title: t(mine ? "sites.mine" : "nav.sites"), icon: "globe", actions: mine ? [searchBox(search)] : [searchBox(search), more, newBtn] });
  p.head.classList.add("sites-head");
  const container = h("div", { class: "card pad0 sites-card" });
  const count = h("p", { class: "small muted", role: "status" });
  // Bulk actions: a bar over the list while something is selected. The
  // selection survives re-renders (the stream re-renders often) and drops
  // sites that went away.
  const bulkBar = h("div", { class: "sites-bulk", hidden: true, role: "region", "aria-label": t("sitelist.selection") });
  // With other nodes linked, the list says whose sites these are, and the
  // other machines' follow, each under its name.
  const here = h("h2", { class: "node-title", hidden: true });
  const others = h("div", { class: "node-others" });
  p.body.append(here, bulkBar, container, count, others);

  let filter = "";
  const selected = new Set();
  let visible = [];
  const subs = [];

  function render() {
    const all = store.siteList();
    if (!store.get("sitesLoaded")) { clear(container).append(loading()); return; }
    const q = filter.trim().toLowerCase();
    const groupOf = (s) => (s.manifest && s.manifest.group) || "";
    const list = (q ? all.filter((s) => s.domain.toLowerCase().includes(q) || siteMode(s).includes(q) || groupOf(s).toLowerCase().includes(q)) : all)
      // Grouped sites first, by group, then the rest; by domain inside.
      .slice().sort((a, b) => (groupOf(a) === "") - (groupOf(b) === "") || groupOf(a).localeCompare(groupOf(b)) || a.domain.localeCompare(b.domain));
    for (const d of [...selected]) if (!store.site(d)) selected.delete(d);
    visible = list.map((s) => s.domain);
    renderBulk();
    clear(container);
    if (all.length === 0) {
      container.append(mine
        ? empty(t("sites.emptyMine"), t("sites.emptyMineHint"), null, "globe")
        : empty(t("sites.emptyTitle"), t("sites.emptyHint"), h("button", { type: "button", class: "btn primary", onClick: newSite }, icon("plus"), t("sites.new")), "globe"));
      count.textContent = "";
      return;
    }
    if (list.length === 0) {
      container.append(empty(t("sites.noMatch"), t("sites.noMatchHint", { q: filter }), null, "search"));
      count.textContent = "";
      return;
    }
    const rows = [];
    let current = null;
    const grouped = list.some((s) => groupOf(s));
    // A column nobody has a value for is left out, not filled with dashes:
    // a house of static sites has no ports, and a fresh one no events.
    const cols = { where: list.some((s) => s.port || s.revision), last: list.some((s) => store.lastEventFor(s.domain)) };
    const span = 5 + (cols.where ? 1 : 0) + (cols.last ? 1 : 0);
    for (const s of list) {
      if (grouped && groupOf(s) !== current) {
        current = groupOf(s);
        rows.push(h("tr", { class: "group-row" }, h("th", { colspan: String(span), scope: "colgroup" }, current || t("sites.ungrouped"))));
      }
      rows.push(row(s, cols));
    }
    const all_ = h("input", { type: "checkbox", "aria-label": t("sites.selectAll") });
    all_.checked = visible.length > 0 && visible.every((d) => selected.has(d));
    all_.indeterminate = !all_.checked && visible.some((d) => selected.has(d));
    all_.addEventListener("change", () => {
      for (const d of visible) { if (all_.checked) selected.add(d); else selected.delete(d); }
      render();
    });
    // Its own table, not the shared responsive one: that one turns every
    // cell into a "Label: value" line on a phone, and a site took six of
    // them, a lone checkbox on top and "Last event: —" at the bottom. Here
    // a row is two lines on a phone (the name with its mode, then its state
    // and when it last did something) and the checkbox sits beside them.
    const tbl = table([
      { label: all_, class: "check-col" },
      { label: t("sites.col.domain") },
      { label: t("sites.col.status") },
      { label: t("sites.col.mode") },
      cols.where ? { label: t("sitelist.col.where") } : null,
      cols.last ? { label: t("sitelist.col.last"), class: "right" } : null,
      { label: h("span", { class: "sr-only" }, t("sitelist.col.chat")), class: "chat-col" },
    ].filter(Boolean), rows).el;
    tbl.querySelector("table").classList.add("sites-table");
    container.classList.toggle("selecting", selected.size > 0);
    container.append(tbl);
    // The total is the list itself; the count only says something when
    // a search has left some out.
    count.textContent = list.length === all.length ? "" : t("sites.count", { n: list.length, total: all.length });
  }

  function row(s, cols) {
    const href = `#/sites/${encodeURIComponent(s.domain)}`;
    const last = store.lastEventFor(s.domain);
    const m = s.manifest || {};
    const box = h("input", { type: "checkbox", "aria-label": t("sites.select", { domain: s.domain }) });
    box.checked = selected.has(s.domain);
    box.addEventListener("click", (e) => e.stopPropagation());
    box.addEventListener("change", () => { if (box.checked) selected.add(s.domain); else selected.delete(s.domain); render(); });
    // Where it answers, for a process: its port and revision, quiet. A
    // static site or a project has neither, and the cell stays empty
    // instead of holding a dash.
    const where = [s.port ? ":" + s.port : "", shortRev(s.revision)].filter(Boolean).join(" · ");
    const tr = h("tr", { class: "row-link" + (box.checked ? " picked" : ""), tabindex: "-1" },
      cell(h("label", { class: "site-pick" }, box), null, "check-col"),
      cell(h("div", { class: "site-name" },
        h("a", { class: "cell-link", href }, s.domain),
        m.title && m.title !== s.domain ? h("span", { class: "site-title" }, m.title) : null), null, "site-cell"),
      cell(s.unreachable ? h("span", { class: "badge warn" }, t("sites.unreachable")) : aliveStatus(s), null, "site-state"),
      // The mode only where it adds to the state: "held, held" and
      // "stopped, stopped" read as a stutter.
      cell(["hold", "stopped"].includes(siteMode(s)) ? "" : modeBadge(s), null, "site-mode"),
      cols.where ? cell(where ? h("span", { class: "mono", title: s.revision || "" }, where) : "", null, "site-where") : null,
      cols.last ? cell(last ? h("span", { class: "site-last", title: [last.type, last.message, fmtTime(last.time)].filter(Boolean).join(" · ") }, fmtAgo(last.time, t)) : "", null, "site-last-cell right") : null,
      cell(can(s.domain, "agents") ? chatLink(s.domain, currentNode() || "") : "", null, "site-chat-cell"),
    );
    tr.addEventListener("click", (e) => {
      if (e.target.closest("a") || e.target.closest("input") || e.target.closest("label")) return;
      navigate(`/sites/${encodeURIComponent(s.domain)}`);
    });
    return tr;
  }

  // Each site's conversation is a tap away from the list, which is how the
  // operator works: open a site's chat, ask, come back to the list. The
  // button says whose turn it is, as the bar's pill does: amber while a
  // conversation of the site waits for an answer, a pulsing dot while its
  // agent works. Tapping it opens that conversation, not merely the last
  // one used. node is the machine the site lives on ("" for this one).
  function chatState(domain, node) {
    const mine = (store.get("liveChats") || []).filter((r) => r.domain === domain && (r.node || "") === node);
    return { asking: mine.find((r) => r.asking), talking: mine.find((r) => !r.asking) };
  }
  function chatLink(domain, node, go) {
    const st = chatState(domain, node);
    const which = st.asking ? "asking" : st.talking ? "talking" : "";
    const label = t(which === "asking" ? "sitelist.chatAsking" : which === "talking" ? "sitelist.chatTalking" : "sitelist.chat", { domain });
    const a = h("a", { class: "chat-quick" + (which ? " " + which : ""), href: `#/sites/${encodeURIComponent(domain)}/chat`, "aria-label": label, title: label },
      icon("chat"), which ? h("span", { class: "chat-quick-dot", "aria-hidden": "true" }) : null);
    a.addEventListener("click", (e) => {
      e.stopPropagation();
      const r = st.asking || st.talking;
      if (r) store.set("openChat", { domain, id: r.id || "" });
      if (go) { e.preventDefault(); go(); }
    });
    return a;
  }

  async function newSite() {
    const domain = h("input", { type: "text", placeholder: "example.com", autocomplete: "off", autocapitalize: "off", spellcheck: "false", required: true });
    const template = h("select", null,
      ...["static", "node", "go", "docker", "project"].map((k) => h("option", { value: k }, t("template." + k))),
    );
    // Publishing is on by default: a site is something people reach. A
    // name without a dot is a project and has nothing to publish.
    const publish = checkbox(t("sites.publish"), { checked: true });
    const isProject = () => !domain.value.includes(".");
    const syncPublish = () => {
      publish.input.disabled = isProject();
      // From a repository the template is the repository's to say.
      if (typeof source !== "undefined" && source.value === "repo") return;
      if (isProject()) template.value = "project";
      else if (template.value === "project") template.value = "static";
    };
    domain.addEventListener("input", syncPublish);
    // A random name from pronounceable words, as many as chosen beside it.
    // What the field holds is kept as the parent domain; see withLabel.
    const words = h("select", { "aria-label": t("sites.randomLength") },
      ...Array.from({ length: MAX_WORDS }, (_, i) => h("option", { value: i + 1 }, i ? t("sites.randomWords", { n: i + 1 }) : t("sites.randomWord"))));
    words.value = "3";
    const roll = h("button", { type: "button", class: "btn", title: t("sites.randomHint") }, icon("sparkles"), t("sites.random"));
    let lastLabel = "";
    const generate = () => {
      const label = randomLabel(words.value);
      domain.value = withLabel(domain.value, label, lastLabel, commonParent(store.siteList().map((s) => s.domain)));
      lastLabel = label;
      syncPublish();
      domain.focus();
    };
    roll.addEventListener("click", generate);
    words.addEventListener("change", () => { if (lastLabel) generate(); });
    const domainField = field(t("sites.domain"), domain, t("sites.domainHint"));
    domainField.insertBefore(h("div", { class: "row domain-row" }, domain, words, roll), domainField.querySelector(".hint"));
    // Where the code comes from: a template, or a GitHub repository (only
    // with the GitHub module on). From a repository, the template is what
    // its root says unless chosen.
    const source = h("select", null, h("option", { value: "template" }, t("sites.fromTemplate")), h("option", { value: "repo" }, t("sites.fromRepo")));
    const repoListID = "repos-" + Math.random().toString(36).slice(2, 8);
    const repo = h("input", { type: "text", list: repoListID, placeholder: t("sites.repoPlaceholder"), autocomplete: "off", spellcheck: "false" });
    const repoList = h("datalist", { id: repoListID });
    const repoHint = h("div", { class: "hint" }, t("sites.repoHint"));
    const branch = h("input", { type: "text", placeholder: t("sites.branchPlaceholder"), autocomplete: "off", spellcheck: "false" });
    // "What the repository says" is an option only while the code comes
    // from a repository. It was always in the list, hidden: prepended to a
    // select with nothing chosen it became the chosen one, so the dialog
    // opened on it with "a template" as the source and Create answered
    // "unknown template"; and iOS shows hidden options in its picker
    // anyway. Out of the list it can be neither.
    const auto = h("option", { value: "auto" }, t("template.auto"));
    const repoFields = h("div", { hidden: true }, h("div", { class: "field" }, h("label", null, t("sites.repo")), repo, repoList, repoHint), field(t("sites.branch"), branch, t("sites.branchHint")));
    let githubOn = false;
    api.get("/modules").then((list) => {
      const gh = (Array.isArray(list) ? list : list.modules || []).find((m) => m.key === "github");
      githubOn = Boolean(gh && gh.enabled);
      source.options[1].disabled = !githubOn;
      if (!githubOn) source.options[1].textContent = t("sites.fromRepoOff");
    }).catch(() => {});
    const syncSource = async () => {
      const fromRepo = source.value === "repo";
      repoFields.hidden = !fromRepo;
      if (fromRepo) template.prepend(auto);
      else auto.remove();
      template.value = fromRepo ? "auto" : (isProject() ? "project" : "static");
      if (fromRepo && !repoList.childElementCount) {
        try {
          const r = await api.get("/modules/github/repos");
          for (const x of r.repos || []) repoList.append(h("option", { value: x.full_name }, (x.private ? "🔒 " : "") + (x.description || "")));
          repoHint.textContent = r.error ? t("sites.repoNoToken") : t("sites.repoHint", { n: (r.repos || []).length });
        } catch (e) { repoHint.textContent = e.message; }
      }
    };
    source.addEventListener("change", syncSource);
    // Where it is made: the node being viewed unless another is chosen.
    const nodes_ = store.get("nodes");
    const where = h("select", null);
    if (nodes_ && (nodes_.members || []).length) {
      where.append(h("option", { value: "" }, t("nodes.thisOne", { name: nodes_.self.name })), ...nodes_.members.map((x) => h("option", { value: x.id }, x.name)));
      where.value = currentNode();
    }
    let published = false;
    const made = await open({
      title: t("sites.newTitle"),
      body: [
        domainField,
        where.options.length ? field(t("sites.node"), where, t("sites.nodeHint")) : null,
        field(t("sites.source"), source),
        repoFields,
        field(t("sites.template"), template, t("sites.templateHint")),
        h("div", { class: "field" }, publish.el, h("div", { class: "hint" }, t("sites.publishHint"))),
      ],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("common.create"), submit: true, primary: true }],
      onSubmit: async (_form, showError) => {
        const d = domain.value.trim().toLowerCase();
        if (!/^[a-z0-9-]+(\.[a-z0-9-]+)*$/.test(d)) { showError(t("sites.badDomain")); return false; }
        const body = { domain: d, template: template.value };
        if (source.value === "repo") {
          if (!repo.value.trim()) { showError(t("sites.repoNeeded")); return false; }
          body.repository = repo.value.trim();
          if (branch.value.trim()) body.branch = branch.value.trim();
        }
        // Only a repository has something to detect; anywhere else "auto"
        // is the server's own default (static, or project without a dot).
        if (body.template === "auto") delete body.template;
        if (!isProject()) body.publish = publish.input.checked;
        const target = where.options.length ? where.value : currentNode();
        const site = await api.at(target).post("/sites", body);
        published = Boolean(site.publishing);
        if (target === currentNode()) store.putSite(site);
        else setCurrentNode(target);
        return site.domain;
      },
    });
    if (made) {
      toast(t(published ? "sites.createdPublishing" : "sites.created", { domain: made }), "ok");
      navigate(`/sites/${encodeURIComponent(made)}`);
    }
  }

  function renderBulk() {
    clear(bulkBar);
    bulkBar.hidden = selected.size === 0;
    if (!selected.size) return;
    const del = h("button", { type: "button", class: "btn sm danger" }, icon("trash"), t("sites.bulkDelete"));
    const none = h("button", { type: "button", class: "btn sm ghost" }, t("sites.selectNone"));
    del.addEventListener("click", bulkDelete);
    none.addEventListener("click", () => { selected.clear(); render(); });
    bulkBar.append(h("strong", { class: "grow" }, t("sites.selected", { n: selected.size })), none, del);
  }

  // One confirmation that names every site and makes the count typed, not
  // clicked: deleting thirty sites must not be one tired Enter. Archiving
  // is on by default, as for one site. They go one at a time and each
  // result is shown; a failure stops nothing else.
  async function bulkDelete() {
    const domains = [...selected].sort();
    const archive = checkbox(t("site.archive"), { checked: true });
    const typed = h("input", { type: "text", inputmode: "numeric", autocomplete: "off" });
    const results = h("div", { class: "stack-sm small" });
    let done = false;
    await open({
      title: t("sites.bulkDeleteQ", { n: domains.length }),
      wide: true,
      body: [
        h("p", null, t("sites.bulkDeleteBody")),
        h("p", { class: "mono small" }, domains.join("  ")),
        h("div", { class: "field" }, archive.el, h("div", { class: "hint" }, t("site.archiveHint"))),
        field(t("sites.bulkDeleteType", { n: domains.length }), typed),
        results,
      ],
      buttons: [{ label: t("common.close"), value: null }, { label: t("sites.bulkDelete"), submit: true, danger: true, primary: true }],
      onSubmit: async (_f, showError) => {
        if (done) return true;
        if (typed.value.trim() !== String(domains.length)) { showError(t("sites.bulkDeleteTypeWrong", { n: domains.length })); return false; }
        showError("");
        clear(results);
        for (const d of domains) {
          const line = h("div", null, h("code", null, d), " … ");
          results.append(line);
          try {
            await api.del(`/sites/${encodeURIComponent(d)}` + (archive.input.checked ? "?archive=true" : ""));
            store.dropSite(d);
            selected.delete(d);
            line.append(h("span", { class: "level-ok" }, archive.input.checked ? t("sites.bulkArchived") : t("sites.bulkDeleted")));
          } catch (e) {
            line.append(h("span", { class: "level-error" }, e.message));
          }
        }
        done = true;
        render();
        return false; // the results are the answer; Close ends it
      },
    });
  }

  newBtn.addEventListener("click", newSite);

  // A site from a V1 house: a token for that one site and two hours, and
  // the command to paste on the V1 machine. The house makes the token; the
  // V1 machine only ever holds that one.
  async function importV1() {
    const domain = h("input", { type: "text", placeholder: "example.com", autocomplete: "off", autocapitalize: "off", spellcheck: "false" });
    const base = h("input", { type: "text", placeholder: "https://panel.example.com", autocomplete: "off", autocapitalize: "off", spellcheck: "false" });
    const made = await open({
      title: t("sites.importV1"),
      body: [h("p", { class: "dim" }, t("sites.importV1Body")), field(t("sites.importV1Domain"), domain), field(t("sites.importV1Base"), base, t("sites.importV1BaseHint"))],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("sites.importV1Make"), submit: true, primary: true }],
      onSubmit: async () => api.post("/relocate/import-token", { domain: domain.value.trim().toLowerCase(), base: base.value.trim() }),
    });
    if (!made) return;
    const code = h("pre", { class: "snippet" }, made.command);
    const copy = h("button", { type: "button", class: "btn" }, icon("copy"), t("door.copy"));
    copy.addEventListener("click", async () => { try { await navigator.clipboard.writeText(made.command); toast(t("door.copied"), "ok"); } catch { /* it stays on screen */ } });
    await open({
      title: t("sites.importV1Ready", { domain: made.domain }),
      body: [h("p", { class: "dim" }, t("sites.importV1Run", { when: new Date(made.expires).toLocaleTimeString() })), code, copy, h("p", { class: "small muted" }, t("sites.importV1After"))],
      buttons: [{ label: t("common.close"), value: null }],
    });
  }
  // Archived sites: one file each under data/archive, faithful (data and
  // dependencies included), restored to sites/ as they were.
  async function showArchived() {
    const list = h("div", { class: "stack-sm" });
    const fill = async () => {
      clear(list);
      const items = await api.get("/archive");
      if (!items.length) { list.append(empty(t("sites.noArchived"), t("sitelist.noArchivedHint"), null, "history")); return; }
      for (const a of items) {
        const restore = h("button", { type: "button", class: "btn sm" }, t("sites.restore"));
        restore.addEventListener("click", async () => {
          restore.classList.add("busy");
          try { const s = await api.post(`/archive/${encodeURIComponent(a.name)}/restore`); store.putSite(s); toast(t("sites.restored", { domain: a.domain }), "ok"); await fill(); }
          catch (e) { toastError(e); } finally { restore.classList.remove("busy"); }
        });
        // An archive is the last copy of a deleted site: one click took it
        // for good, so it asks first, naming the site.
        const remove = h("button", { type: "button", class: "btn sm ghost danger", "aria-label": t("common.delete"), title: t("common.delete") }, icon("trash"));
        remove.addEventListener("click", async () => {
          if (!(await confirm({ title: t("sitelist.archiveDeleteQ", { domain: a.domain }), body: t("sitelist.archiveDeleteBody"), confirmLabel: t("common.delete"), danger: true }))) return;
          try { await api.del(`/archive/${encodeURIComponent(a.name)}`); await fill(); } catch (e) { toastError(e); }
        });
        list.append(h("div", { class: "archive-row" },
          h("div", { class: "grow" }, h("strong", null, a.domain), h("div", { class: "small muted", title: new Date(a.at).toLocaleString() }, fmtAgo(a.at, t), " · ", fmtBytes(a.bytes))),
          h("a", { class: "btn sm ghost", href: apiURL(`/archive/${encodeURIComponent(a.name)}/download`), download: a.name, "aria-label": t("site.download"), title: t("site.download") }, icon("download")),
          restore, remove));
      }
    };
    try { await fill(); } catch (e) { toastError(e); return; }
    await open({ title: t("sites.archivedTitle"), body: [h("p", { class: "dim" }, t("sites.archivedBody")), list], buttons: [{ label: t("common.close"), value: null }] });
  }
  search.addEventListener("input", () => { filter = search.value; render(); paintOthers(); });

  // The other nodes: what was chosen in the selector is above, live; the
  // rest are read every twenty seconds through this node.
  function nodeList() {
    const n = store.get("nodes");
    if (!n || !(n.members || []).length) return [];
    const list = currentNode() ? [{ id: "", name: n.self.name }] : [];
    for (const peer of n.members) if (peer.id !== currentNode()) list.push(peer);
    return list;
  }
  function paintHere() {
    const n = store.get("nodes");
    const peers = (n && n.members) || [];
    here.hidden = peers.length === 0;
    if (here.hidden) return;
    const name = currentNode() ? (peers.find((x) => x.id === currentNode()) || {}).name : n.self.name;
    clear(here).append(...[icon("server"), h("span", null, name), currentNode() ? null : h("span", { class: "badge info" }, t("dash.thisNode"))].filter(Boolean));
  }
  let otherSites = [];
  async function loadOthers() {
    const list = nodeList();
    otherSites = await Promise.all(list.map(async (node) => {
      try { return { node, sites: await api.at(node.id).get("/sites") }; } catch (e) { return { node, error: e.message }; }
    }));
    paintOthers();
  }
  function paintOthers() {
    clear(others);
    const q = filter.trim().toLowerCase();
    for (const { node, sites, error } of otherSites) {
      const card = h("section", { class: "card node-section" }, h("h2", null, icon("server"), h("span", null, node.name)));
      if (error) { card.append(notice("bad", error)); others.append(card); continue; }
      const list = (sites || []).filter((x) => !q || x.domain.toLowerCase().includes(q) || siteMode(x).includes(q)).sort((a, b) => a.domain.localeCompare(b.domain));
      if (!list.length) { card.append(h("p", { class: "small muted" }, q ? t("sites.noMatch") : t("sites.emptyTitle"))); others.append(card); continue; }
      const open_ = (x) => { setCurrentNode(node.id); navigate(`/sites/${encodeURIComponent(x.domain)}`); };
      const tw = table([
        { label: t("sites.col.domain") }, { label: t("sites.col.status") }, { label: t("sites.col.mode") }, { label: t("sitelist.col.where") },
        { label: h("span", { class: "sr-only" }, t("sitelist.col.chat")), class: "chat-col" },
      ], list.map((x) => {
        const a = h("a", { class: "cell-link", href: "#" }, x.domain);
        a.addEventListener("click", (e) => { e.preventDefault(); open_(x); });
        const title = (x.manifest || {}).title;
        const where = [x.port ? ":" + x.port : "", shortRev(x.revision)].filter(Boolean).join(" · ");
        const tr = h("tr", { class: "row-link", tabindex: "-1" },
          cell(h("div", { class: "site-name" }, a, title && title !== x.domain ? h("span", { class: "site-title" }, title) : null), null, "site-cell"),
          cell(aliveStatus(x), null, "site-state"), cell(modeBadge(x), null, "site-mode"),
          cell(where ? h("span", { class: "mono" }, where) : "", null, "site-where"),
          cell(chatLink(x.domain, node.id || "", () => { setCurrentNode(node.id); navigate(`/sites/${encodeURIComponent(x.domain)}/chat`); }), null, "site-chat-cell"));
        tr.addEventListener("click", (e) => { if (!e.target.closest("a")) open_(x); });
        return tr;
      })).el;
      tw.querySelector("table").classList.add("sites-table");
      card.append(tw);
      others.append(card);
    }
  }
  subs.push(store.subscribe("nodes", () => { paintHere(); loadOthers(); }));
  paintHere();
  loadOthers();
  const othersTimer = setInterval(() => { if (!document.hidden) loadOthers(); }, 20000);
  subs.push(() => clearInterval(othersTimer));

  // A handover emits several events in a second; one render after the
  // burst is enough and keeps the row from flickering under the pointer.
  const rerender = debounce(render, 80);
  subs.push(store.subscribe("sites", rerender));
  subs.push(store.subscribe("events", () => { if (store.get("sitesLoaded")) rerender(); }));
  // Whose turn it is in each conversation moves the chat buttons.
  subs.push(store.subscribe("liveChats", () => { rerender(); paintOthers(); }));

  (async () => {
    try {
      const list = await api.get("/sites");
      store.setSites(list);
    } catch (e) {
      clear(container).append(errorBox(e));
      toastError(e);
    }
  })();
  render();
  if (query && query.get("focus") === "search") setTimeout(() => search.focus(), 0);

  return { el: p.el, destroy() { subs.forEach((u) => u()); } };
}
