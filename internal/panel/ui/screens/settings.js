// Settings: what this installation decided that is not about one site.
// The server part is data/settings.json through GET/PUT; theme and
// language are this browser's and never leave it.

import { icon } from "../lib/icons.js";
import { h, clear, copy } from "../lib/dom.js";
import { t, LANGUAGES, LANGUAGE_NAMES, currentLang } from "../lib/i18n.js";
import { api, apiURL, enc } from "../lib/api.js";
import { page, load, table, cell } from "../components/page.js";
import { open, confirm, field, toggle, checkbox } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { getTheme, setTheme, setLang } from "../lib/theme.js";
import { fmtTime, fmtAgo, fmtBytes } from "../lib/dom.js";
import { passkeysCard, panelAccessCard, panelKeyCard } from "./settings-access.js";
import { usersCard } from "./settings-users.js";
import { pushCard } from "./settings-push.js";
import { autofixCard, automationCard } from "./settings-autofix.js";
import { cardHead, saveBar, savedNote, fold, quietEmpty } from "./settings-kit.js";

// The screen's sections, in the order the nav shows them. Fifteen cards
// on one page were 5,700px on a phone, and the card an operator came for
// was always somewhere below the fold; a section holds the cards that are
// changed together, and only one section is on screen at a time.
const SECTIONS = [
  { key: "install", icon: "server" },
  { key: "notify", icon: "bell" },
  { key: "agents", icon: "sparkles" },
  { key: "access", icon: "lock" },
  { key: "browser", icon: "monitor" },
];

export function settingsScreen({ query } = {}) {
  const p = page({ title: t("nav.settings"), icon: "sliders" });
  const general = h("div", { class: "card set-card" });
  const pagesCard = h("div", { class: "card set-card" });
  const cacheCard = h("div", { class: "card set-card" });
  const agentsCard = h("div", { class: "card set-card" });
  const tokensCard = h("div", { class: "card pad0 set-card" });
  const depsCard = h("div", { class: "card set-card" });
  const localCard = h("div", { class: "card set-card" });
  const cards = {
    install: [general, pagesCard, cacheCard],
    notify: [pushCard()],
    agents: [agentsCard, automationCard(), autofixCard(), depsCard],
    access: [passkeysCard(), panelAccessCard(), panelKeyCard(), usersCard(), tokensCard],
    browser: [localCard],
  };

  // The nav: a column beside the cards on a wide screen, a row of pills
  // under the title on a phone. The section is in the address (?s=), so a
  // reload or a link lands on it; replaceState keeps it out of the back
  // button, which leaves Settings rather than walking back through tabs.
  const tabs = [];
  const panels = {};
  const nav = h("nav", { class: "set-nav", role: "tablist", "aria-label": t("settings.sections") });
  const content = h("div", { class: "set-panels" });
  for (const sec of SECTIONS) {
    const id = "set-" + sec.key;
    const tab = h("button", { type: "button", role: "tab", id: id + "-tab", "aria-controls": id, class: "set-tab" },
      icon(sec.icon), h("span", null, t("settings.section." + sec.key)));
    tab.addEventListener("click", () => show(sec.key, true));
    tabs.push(tab);
    nav.append(tab);
    panels[sec.key] = h("section", { class: "set-panel", role: "tabpanel", id, "aria-labelledby": id + "-tab", hidden: true },
      h("p", { class: "set-panel-sub" }, t("settings.sectionSub." + sec.key)), ...cards[sec.key]);
    content.append(panels[sec.key]);
  }
  // Arrows move along the tabs, as a tab list does everywhere else.
  nav.addEventListener("keydown", (e) => {
    const i = tabs.indexOf(document.activeElement);
    const step = { ArrowRight: 1, ArrowDown: 1, ArrowLeft: -1, ArrowUp: -1 }[e.key];
    if (i < 0 || !step) return;
    e.preventDefault();
    const next = tabs[(i + step + tabs.length) % tabs.length];
    next.focus();
    next.click();
  });
  function show(key, byHand) {
    if (!panels[key]) key = SECTIONS[0].key;
    SECTIONS.forEach((sec, i) => {
      const on = sec.key === key;
      tabs[i].setAttribute("aria-selected", String(on));
      tabs[i].tabIndex = on ? 0 : -1;
      panels[sec.key].hidden = !on;
    });
    if (!byHand) return;
    history.replaceState(null, "", "#/settings?s=" + key);
    // On a phone the pills scroll sideways: the chosen one comes into
    // view. A section opened from far down a long one starts at its top.
    const tab = tabs[SECTIONS.findIndex((x) => x.key === key)];
    nav.scrollTo({ left: tab.offsetLeft - nav.clientWidth / 2 + tab.clientWidth / 2, behavior: "smooth" });
    if (window.scrollY > p.body.offsetTop) window.scrollTo({ top: 0 });
  }
  p.body.append(h("div", { class: "set-layout" }, nav, content));
  show(query && query.get("s"));

  // The deps card saves on its own, like the agents one.
  async function renderDeps() {
    const s = await api.get("/settings");
    const on = !(s.deps && s.deps.auto_update === false);
    const sw = toggle(t("settings.depsAutoUpdate"), { checked: on });
    const note = savedNote();
    sw.input.addEventListener("change", async () => {
      sw.input.disabled = true;
      try {
        const cur = await api.get("/settings");
        await api.put("/settings", { ...cur, deps: { ...(cur.deps || {}), auto_update: sw.input.checked } });
        note.saved();
      } catch (e) { sw.input.checked = !sw.input.checked; toastError(e); }
      finally { sw.input.disabled = false; }
    });
    clear(depsCard).append(cardHead(t("settings.deps"), t("settings.depsHint")), h("div", { class: "set-switch-row" }, sw.el, note.el));
    return null;
  }

  // The agents card saves on its own: the general form keeps whatever the
  // agents section holds and this one keeps whatever the general one holds,
  // each re-reading the file before it writes so neither overwrites the
  // other's last save with stale fields.
  async function renderAgents() {
    const [s, agents] = await Promise.all([api.get("/settings"), api.get("/agents").catch(() => ({ providers: [] }))]);
    const a = s.agents || {};
    const providers = (agents.providers || []).map((x) => x.provider);
    for (const k of Object.keys(a.per_provider || {})) if (!providers.includes(k)) providers.push(k);
    if (a.default_provider && !providers.includes(a.default_provider)) providers.push(a.default_provider);
    // Empty fields are computed from the machine: the placeholder is the
    // number that applies, and the hint says where it came from.
    const comp = agents.computed || {};
    const max = h("input", { type: "number", min: "1", max: "64", value: a.max_concurrent || "", placeholder: comp.max_concurrent ? String(comp.max_concurrent) : "" });
    const cores = h("input", { type: "number", min: "1", max: comp.cpus ? String(comp.cpus) : "", value: a.cpu_cores || "", placeholder: comp.cpu_cores ? String(comp.cpu_cores) : "" });
    const memMax = h("input", { type: "text", class: "mono", value: a.memory_max || "", placeholder: comp.memory_max ? fmtBytes(comp.memory_max) : "", autocapitalize: "off" });
    const reserved = h("input", { type: "number", min: "0", max: "63", value: a.reserved_for_house ?? 1 });
    const def = h("select", null, ...providers.map((k) => h("option", { value: k, selected: k === a.default_provider }, k)));
    // What a site may spend a day through its door when its manifest says
    // nothing (claude_api.daily_usd); empty is the house's default.
    const siteDaily = h("input", { type: "number", min: "0", step: "0.5", value: a.site_daily_usd || "", placeholder: "5" });
    const per = {};
    const perFields = providers.map((k) => {
      per[k] = h("input", { type: "number", min: "0", max: "64", value: (a.per_provider && a.per_provider[k]) || 0 });
      return field(t("settings.perProviderCap", { provider: k }), per[k], t("settings.perProviderHint"));
    });
    const save = h("button", { type: "submit", class: "btn primary" }, t("common.save"));
    const err = h("div", { class: "error-box", hidden: true });
    const bar = saveBar(save);
    // The cap and the default provider are what gets changed; the
    // reserved slots, a door's daily dollars and each provider's own cap
    // are right for nearly everyone and fold, saying what they hold.
    const capped = providers.filter((k) => a.per_provider && a.per_provider[k]);
    const moreNote = [
      t("settings.reservedNote", { n: a.reserved_for_house ?? 1 }),
      t("settings.siteDailyNote", { usd: a.site_daily_usd || 5 }),
      capped.length ? t("settings.capsNote", { list: capped.map((k) => `${k} ${a.per_provider[k]}`).join(", ") }) : t("settings.noCapsNote"),
    ].join(" · ");
    const form = h("form", { novalidate: true },
      h("div", { class: "form-row" },
        field(t("settings.maxConcurrent"), max, a.max_concurrent ? t("settings.maxConcurrentHint") : t("settings.maxConcurrentComputed", { n: comp.max_concurrent || "?" })),
        field(t("settings.defaultProvider"), def, t("settings.defaultProviderHint")),
      ),
      fold(t("settings.advanced"), moreNote, [
        h("div", { class: "form-row" },
          field(t("settings.reservedForHouse"), reserved, t("settings.reservedForHouseHint")),
          field(t("settings.siteDailyUSD"), siteDaily, t("settings.siteDailyUSDHint"))),
        h("div", { class: "form-row" }, ...perFields),
        h("div", { class: "form-row" },
          field(t("settings.agentsCores"), cores, t("settings.agentsCoresHint", { n: comp.cpu_cores || "?", cpus: comp.cpus || "?" })),
          field(t("settings.agentsMemory"), memMax, t("settings.agentsMemoryHint", { size: comp.memory_max ? fmtBytes(comp.memory_max) : "?" }))),
      ]),
      err,
      bar.el,
    );
    bar.watch(form);
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      err.hidden = true;
      // An empty cap is 0, computed; the reserved slots are then held to
      // the computed number, which the server checks again.
      const m = max.value.trim() === "" ? 0 : Number(max.value), r = Number(reserved.value);
      const eff = m || comp.max_concurrent || 64;
      const c = cores.value.trim() === "" ? 0 : Number(cores.value);
      const perOut = {};
      let bad = !(Number.isInteger(m) && m >= 0 && m <= 64 && Number.isInteger(r) && r >= 0 && r < eff && Number.isInteger(c) && c >= 0);
      for (const k of providers) {
        const v = Number(per[k].value);
        if (!(Number.isInteger(v) && v >= 0 && v <= 64)) bad = true;
        perOut[k] = v;
      }
      const daily = siteDaily.value.trim() === "" ? 0 : Number(siteDaily.value);
      if (!(Number.isFinite(daily) && daily >= 0)) bad = true;
      if (bad) { err.textContent = t("settings.agentsBad"); err.hidden = false; return; }
      save.classList.add("busy");
      try {
        const cur = await api.get("/settings");
        // Spread over what is saved, so a field this card does not show
        // survives its save instead of being dropped.
        await api.put("/settings", { ...cur, agents: { ...(cur.agents || {}), max_concurrent: m, reserved_for_house: r, per_provider: perOut, default_provider: def.value, site_daily_usd: daily, cpu_cores: c, memory_max: memMax.value.trim() } });
        bar.saved(t("settings.inForce"));
      } catch (ex) { err.textContent = ex.message; err.hidden = false; }
      finally { save.classList.remove("busy"); }
    });
    clear(agentsCard).append(cardHead(t("settings.agents"), t("settings.agentsHint")), form);
    return null;
  }

  async function renderServer() {
    const s = await api.get("/settings");
    const panelDomain = h("input", { type: "text", class: "mono", value: s.panel_domain || "", placeholder: "panel.example.com", autocapitalize: "off" });
    const listen = h("input", { type: "text", class: "mono", value: s.listen || "", placeholder: "127.0.0.1:46100" });
    const from = h("input", { type: "number", min: "1024", max: "65535", value: (s.ports && s.ports.from) || "" });
    const to = h("input", { type: "number", min: "1024", max: "65535", value: (s.ports && s.ports.to) || "" });
    const save = h("button", { type: "submit", class: "btn primary" }, t("common.save"));
    const err = h("div", { class: "error-box", hidden: true });
    const bar = saveBar(save);
    // The domain is what most installations set here; the listen address
    // and the port range are the installer's, and fold saying what they are.
    const moreNote = [s.listen || "", s.ports && s.ports.from ? t("settings.portsNote", { from: s.ports.from, to: s.ports.to }) : ""].filter(Boolean).join(" · ");
    const form = h("form", { novalidate: true },
      field(t("settings.panelDomain"), panelDomain, t("settings.panelDomainHint")),
      fold(t("settings.advanced"), moreNote, [
        field(t("settings.listen"), listen, t("settings.listenHint")),
        h("div", { class: "form-row" }, field(t("settings.portFrom"), from), field(t("settings.portTo"), to)),
        h("p", { class: "hint" }, t("settings.portsHint")),
      ]),
      err,
      bar.el,
    );
    bar.watch(form);
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      err.hidden = true;
      const f = Number(from.value), tt = Number(to.value);
      if (!(f > 0 && tt > f)) { err.textContent = t("settings.portsBad"); err.hidden = false; return; }
      const cur = await api.get("/settings").catch(() => s);
      const out = { ...cur, panel_domain: panelDomain.value.trim(), listen: listen.value.trim(), ports: { from: f, to: tt } };
      save.classList.add("busy");
      try { await api.put("/settings", out); bar.saved(t("settings.atRestart")); }
      catch (ex) { err.textContent = ex.message; err.hidden = false; }
      finally { save.classList.remove("busy"); }
    });
    // Named for what it holds: "Installation" was also the name of the
    // section it sits in.
    clear(general).append(cardHead(t("settings.panelAddress"), t("settings.generalHint")), form);
    renderPages(s);
    renderCache(s);
    return null;
  }

  // The visitor pages' design, chosen by seeing it: the frame shows the
  // page a visitor gets, in the design selected, before it is saved.
  async function renderPages(s) {
    let cat = { themes: [] };
    try { cat = await api.get("/pages/themes"); } catch { /* the select stays empty */ }
    const theme = h("select", null, ...cat.themes.map((th) => h("option", { value: th.key }, th.name)));
    theme.value = s.visitor_theme || cat.house || "sober";
    const page = h("select", null, ...["down", "starting", "stopped", "hold", "forbidden", "password"].map((k) => h("option", { value: k }, t("settings.pagePreview." + k))));
    const note = h("span");
    // Same origin so the session cookie goes with it; no scripts: the page
    // has none, and its own policy allows none either.
    const frame = h("iframe", { class: "page-preview", title: t("settings.pagesPreview"), sandbox: "allow-same-origin" });
    const show = () => {
      const th = cat.themes.find((x) => x.key === theme.value);
      // The note in the panel's language when it has it, the server's otherwise.
      const k = "pages.themeNote." + theme.value;
      note.textContent = t(k) !== k ? t(k) : (th ? th.note : "");
      frame.src = apiURL(`/pages/preview?theme=${encodeURIComponent(theme.value)}&page=${encodeURIComponent(page.value)}`);
    };
    theme.addEventListener("change", show);
    page.addEventListener("change", show);
    const save = h("button", { type: "button", class: "btn primary" }, t("common.save"));
    const bar = saveBar(save);
    // Only the design is saved: which page the frame shows is for looking.
    theme.addEventListener("change", () => bar.dirty());
    save.addEventListener("click", async () => {
      save.classList.add("busy");
      try { const cur = await api.get("/settings"); await api.put("/settings", { ...cur, visitor_theme: theme.value }); bar.saved(); }
      catch (e) { toastError(e); } finally { save.classList.remove("busy"); }
    });
    clear(pagesCard).append(cardHead(t("settings.pages"), t("settings.pagesHint")),
      h("div", { class: "form-row" }, field(t("settings.pagesTheme"), theme, note), field(t("settings.pagesPage"), page)),
      frame, bar.el);
    show();
  }

  // The memory every site's response cache shares.
  function renderCache(s) {
    const mb = h("input", { type: "number", min: "16", step: "16", value: String((s.cache && s.cache.max_mb) || 256), class: "set-num" });
    const save = h("button", { type: "button", class: "btn primary" }, t("common.save"));
    const bar = saveBar(save, mb);
    save.addEventListener("click", async () => {
      save.classList.add("busy");
      try { const cur = await api.get("/settings"); await api.put("/settings", { ...cur, cache: { ...(cur.cache || {}), max_mb: Number(mb.value) } }); bar.saved(); }
      catch (e) { toastError(e); } finally { save.classList.remove("busy"); }
    });
    clear(cacheCard).append(cardHead(t("settings.cacheTitle"), t("settings.cacheHint")), field(t("settings.cacheMB"), mb), bar.el);
  }

  async function renderTokens() {
    const tokens = await api.get("/auth/tokens");
    const newBtn = h("button", { type: "button", class: "btn sm primary", "data-shortcut": "new" }, icon("plus"), t("settings.newToken"));
    newBtn.addEventListener("click", newToken);
    const rows = (tokens || []).map((tk) => h("tr", null,
      cell(tk.name || tk.id, null, "primary"),
      cell(h("code", { class: "small" }, tk.id), t("settings.tokenId")),
      cell((tk.scopes || []).length ? h("code", { class: "small" }, tk.scopes.join(", ")) : h("span", { class: "muted" }, t("settings.tokenFull")), t("settings.tokenOpens")),
      cell(tk.created ? h("span", { title: fmtTime(tk.created) }, fmtAgo(tk.created, t)) : "", t("settings.created")),
      cell(tk.last_used ? h("span", { title: fmtTime(tk.last_used) }, fmtAgo(tk.last_used, t)) : h("span", { class: "muted" }, t("settings.neverUsed")), t("settings.lastUsed")),
      cell(revokeBtn(tk), null, "right"),
    ));
    clear(tokensCard).append(
      h("div", { class: "card-pad" }, cardHead(t("settings.tokens"), t("settings.tokensHint"), newBtn)),
      rows.length ? table([{ label: t("settings.tokenName") }, { label: t("settings.tokenId") }, { label: t("settings.tokenOpens") }, { label: t("settings.created") }, { label: t("settings.lastUsed") }, { label: "" }], rows, { responsive: true }).el
        : quietEmpty("code", t("settings.noTokens"), t("settings.noTokensHint")),
    );
    return null;
  }
  function revokeBtn(tk) {
    const b = h("button", { type: "button", class: "btn sm danger ghost" }, t("settings.revoke"));
    b.addEventListener("click", async () => {
      if (!(await confirm({ title: t("settings.revokeQ", { name: tk.name || tk.id }), body: t("settings.revokeBody"), confirmLabel: t("settings.revoke"), danger: true }))) return;
      try { await api.del(`/auth/tokens/${enc(tk.id)}`); toast(t("settings.revoked"), "ok"); load(tokensCard, renderTokens); } catch (e) { toastError(e); }
    });
    return b;
  }
  async function newToken() {
    const name = h("input", { type: "text", placeholder: "ci", required: true });
    // Full access, or a service door: some of read, restart and deploy,
    // for every site or for one.
    const kind = h("select", null, h("option", { value: "" }, t("settings.tokenKindFull")), h("option", { value: "scoped" }, t("settings.tokenKindScoped")));
    const actions = ["read", "restart", "deploy", "tasks", "backups"].map((a) => ({ a, box: checkbox(t("settings.scope." + a), {}) }));
    const site = h("input", { type: "text", class: "mono", placeholder: "shop.example", autocomplete: "off" });
    const scopedBox = h("div", { hidden: true }, h("div", { class: "field" }, ...actions.map(({ box }) => h("div", null, box.el))), field(t("settings.tokenSite"), site, t("settings.tokenSiteHint")));
    kind.addEventListener("change", () => { scopedBox.hidden = kind.value !== "scoped"; });
    const r = await open({
      title: t("settings.newToken"),
      body: [field(t("settings.tokenName"), name, t("settings.tokenNameHint")), field(t("settings.tokenKind"), kind, t("settings.tokenKindHint")), scopedBox],
      buttons: [{ label: t("common.cancel"), value: null }, { label: t("common.create"), submit: true, primary: true }],
      onSubmit: async (_f, showError) => {
        if (!name.value.trim()) { showError(t("settings.tokenNameRequired")); return false; }
        const body = { name: name.value.trim() };
        if (kind.value === "scoped") {
          const d = site.value.trim().toLowerCase();
          body.scopes = actions.filter(({ box }) => box.input.checked).map(({ a }) => (d ? `${a}:${d}` : a));
          if (!body.scopes.length) { showError(t("settings.tokenScopeRequired")); return false; }
        }
        return api.post("/auth/tokens", body);
      },
    });
    if (!r || !r.token) return;
    const code = h("code", null, r.token);
    const copyBtn = h("button", { type: "button", class: "btn sm" }, icon("copy"), t("common.copy"));
    copyBtn.addEventListener("click", async () => { copyBtn.textContent = (await copy(r.token)) ? t("common.copied") : t("common.copyFailed"); });
    await open({
      title: t("settings.tokenCreated"),
      body: [h("p", { class: "notice warn" }, t("settings.tokenOnce")), h("div", { class: "token-box" }, code, copyBtn)],
      buttons: [{ label: t("common.done"), value: true, primary: true }],
    });
    load(tokensCard, renderTokens);
  }

  // ---- local ----
  const theme = h("select", { "aria-label": t("settings.theme") }, ...["auto", "light", "dark"].map((v) => h("option", { value: v, selected: getTheme() === v }, t("theme." + v))));
  theme.addEventListener("change", () => setTheme(theme.value));
  const lang = h("select", { "aria-label": t("settings.language") }, ...LANGUAGES.map((v) => h("option", { value: v, selected: currentLang() === v }, LANGUAGE_NAMES[v] || v)));
  lang.addEventListener("change", () => { setLang(lang.value); location.reload(); });
  localCard.append(cardHead(t("settings.thisBrowser"), t("settings.themeHint")),
    h("div", { class: "form-row" }, field(t("settings.theme"), theme), field(t("settings.language"), lang, t("settings.languageHint"))));

  load(general, renderServer);
  load(agentsCard, renderAgents);
  load(depsCard, renderDeps);
  load(tokensCard, renderTokens);
  return { el: p.el, destroy() {} };
}
