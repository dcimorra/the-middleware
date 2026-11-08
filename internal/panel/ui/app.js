// The shell: strings, session, navigation, the one event stream, and the
// routes. Screens live in ./screens and know nothing about each other;
// what they share is in ./lib and ./components.

import { h, clear, debounce } from "./lib/dom.js";
import { t, loadStrings } from "./lib/i18n.js";
import { api, onUnauthorized } from "./lib/api.js";
import { store } from "./lib/store.js";
import { connect, disconnect, onEvent } from "./lib/stream.js";
import { route, mount, navigate, currentPath, render as rerender } from "./lib/router.js";
import { applyTheme, getTheme, setTheme, getLang } from "./lib/theme.js";
import { installShortcuts, showShortcuts, GOTO } from "./components/shortcuts.js";
import { toast } from "./components/toast.js";
import { icon } from "./lib/icons.js";
import { authScreen } from "./screens/auth.js";
import { sitesScreen } from "./screens/sites.js";
import { siteScreen } from "./screens/site.js";
import { inboundScreen } from "./screens/inbound.js";
import { wafScreen } from "./screens/waf.js";
import { modulesScreen } from "./screens/modules.js";
import { tasksScreen } from "./screens/tasks.js";
import { eventsScreen } from "./screens/events.js";
import { guardScreen } from "./screens/guard.js";
import { reposScreen } from "./screens/repos.js";
import { docsScreen } from "./screens/docs.js";
import { auditScreen } from "./screens/audit.js";
import { edgeScreen } from "./screens/edge.js";
import { openBanner } from "./screens/settings-access.js";
import { settingsScreen } from "./screens/settings.js";
import { systemScreen } from "./screens/system.js";
import { houseScreen } from "./screens/house.js";
import { llmUsageScreen } from "./screens/llm-usage.js";
import { dashboardScreen } from "./screens/dashboard.js";
import { movesPill } from "./components/moves.js";
import { liveChats } from "./components/live-chats.js";
import { currentNode, setCurrentNode, onNodeChange } from "./lib/node.js";
import { delegate } from "./lib/access.js";
import { passwordDialog } from "./screens/account.js";

const app = document.getElementById("app");
const main = document.getElementById("main");
const sidebar = document.getElementById("sidebar");
const topbar = document.getElementById("topbar");
let pills = null;
const backdrop = document.getElementById("backdrop");
const menuBtn = document.getElementById("menu-btn");
const themeBtn = document.getElementById("theme-btn");
const helpBtn = document.getElementById("help-btn");
const streamState = document.getElementById("stream-state");
const nodePick = document.getElementById("node-pick");
const nodeSelect = document.getElementById("node-select");

applyTheme();

// Each entry: key, route, shortcut, icon. Two groups: what the operator
// runs (sites, doors, protection, modules, tasks) and what they watch
// (events, guard, settings, the process); the sidebar draws the line.
const NAV = [
  ["dashboard", "/dashboard", "g n", "layout"],
  ["sites", "/sites", "g s", "globe"],
  ["inbound", "/inbound", "g i", "door"],
  ["waf", "/waf", "g w", "shield"],
  ["modules", "/modules", "g m", "blocks"],
  ["tasks", "/tasks", "g t", "clock"],
  ["repos", "/repos", "g r", "branch"],
  ["audit", "/audit", "g a", "alert"],
  ["edge", "/edge", "g x", "link"],
  ["house", "/house", "g h", "sparkles"],
  ["llm", "/llm", "g u", "gauge"],
  ["events", "/events", "g e", "activity"],
  ["guard", "/guard", "g g", "eye"],
  ["settings", "/settings", "g c", "sliders"],
  ["system", "/system", "g y", "cpu"],
  ["docs", "/docs", "g d", "file"],
];
const NAV_GROUPS = { dashboard: "nav.groupManage", events: "nav.groupObserve" };

// On a phone the sidebar is a sheet over the screen: it has its own head
// (the name and a way to close it that is not "tap the dark part") and,
// at its foot, the theme, which left the crowded bar for it.
let sheetTheme = null;
function buildSidebar() {
  clear(sidebar);
  const close = h("button", { type: "button", class: "icon-btn", "aria-label": t("common.close") }, icon("x"));
  close.addEventListener("click", () => closeMenu(true));
  sidebar.append(h("div", { class: "sheet-head" }, h("span", { class: "brand" }, brandMark(), h("span", null, t("app.title"))), close));
  // A delegated user has their sites and nothing else of the house.
  const nav = delegate() ? NAV.filter(([key]) => key === "sites") : NAV;
  for (const [key, href, k, ico] of nav) {
    if (NAV_GROUPS[key] && !delegate()) sidebar.append(h("div", { class: "group" }, t(NAV_GROUPS[key])));
    sidebar.append(h("a", { href: "#" + href, "data-path": href },
      icon(ico), h("span", { class: "label" }, t("nav." + key)), h("span", { class: "k", "aria-hidden": "true" }, k)));
  }
  sidebar.append(h("div", { class: "group" }, t("nav.session")));
  if (delegate()) {
    const me = store.get("me");
    sidebar.append(h("div", { class: "small muted signed-as" }, t("account.signedAs", { name: me.user.name })));
    const pw = h("a", { href: "#/password" }, icon("key"), h("span", { class: "label" }, t("account.changePassword")));
    pw.addEventListener("click", (e) => { e.preventDefault(); closeMenu(); passwordDialog(() => showAuth("login")); });
    sidebar.append(pw);
  }
  const logout = h("a", { href: "#/logout" }, icon("logout"), h("span", { class: "label" }, t("auth.signOut")));
  logout.addEventListener("click", async (e) => {
    e.preventDefault();
    try { await api.post("/auth/logout", {}, { allow401: true }); } catch { /* the cookie is gone either way */ }
    showAuth("login");
  });
  sidebar.append(logout);
  sheetTheme = h("button", { type: "button", class: "sheet-theme" });
  sheetTheme.addEventListener("click", cycleTheme);
  sidebar.append(h("div", { class: "sheet-foot" }, sheetTheme));
  themeLabel();
  markNav();
}

// The mark as the bar draws it (index.html), for the sheet's head.
function brandMark() {
  return document.querySelector(".topbar .brand .mark").cloneNode(true);
}

function markNav() {
  const path = currentPath();
  for (const a of sidebar.querySelectorAll("a[data-path]")) {
    const on = path === a.dataset.path || path.startsWith(a.dataset.path + "/") || path.startsWith(a.dataset.path + "?");
    if (on) a.setAttribute("aria-current", "page"); else a.removeAttribute("aria-current");
  }
  closeMenu();
}

// The page under an open sheet does not scroll (a swipe on the menu moved
// the screen behind it), Escape closes it, and focus goes into it and,
// when it is closed by hand, back to the button that opened it.
function openMenu() {
  sidebar.classList.add("open");
  backdrop.hidden = false;
  document.body.classList.add("menu-open");
  menuBtn.setAttribute("aria-expanded", "true");
  const here = sidebar.querySelector("a[aria-current=page]") || sidebar.querySelector("a");
  if (here) here.focus({ preventScroll: true });
}
function closeMenu(refocus) {
  const was = sidebar.classList.contains("open");
  sidebar.classList.remove("open");
  backdrop.hidden = true;
  document.body.classList.remove("menu-open");
  menuBtn.setAttribute("aria-expanded", "false");
  if (was && refocus === true) menuBtn.focus();
}
menuBtn.addEventListener("click", () => (sidebar.classList.contains("open") ? closeMenu(true) : openMenu()));
backdrop.addEventListener("click", () => closeMenu(true));
document.addEventListener("keydown", (e) => { if (e.key === "Escape" && sidebar.classList.contains("open")) closeMenu(true); });

function themeLabel() {
  const v = getTheme();
  const ico = v === "dark" ? "moon" : v === "light" ? "sun" : "monitor";
  clear(themeBtn).append(icon(ico));
  themeBtn.setAttribute("aria-label", t("theme.button", { theme: t("theme." + v) }));
  themeBtn.title = themeBtn.getAttribute("aria-label");
  if (sheetTheme) {
    clear(sheetTheme).append(icon(ico), h("span", { class: "label" }, t("shell.theme")), h("span", { class: "value" }, t("theme." + v)));
    sheetTheme.setAttribute("aria-label", themeBtn.getAttribute("aria-label"));
  }
  // The phone's own bar (and an installed panel's title bar) takes the
  // colour of the panel's bar, not the brand blue over a white one.
  const meta = document.querySelector("meta[name=theme-color]");
  const surface = getComputedStyle(document.documentElement).getPropertyValue("--surface").trim();
  if (meta && surface) meta.content = surface;
}
function cycleTheme() {
  const order = ["auto", "light", "dark"];
  setTheme(order[(order.indexOf(getTheme()) + 1) % order.length]);
  themeLabel();
}
themeBtn.addEventListener("click", cycleTheme);
// "System" follows the device, which can change under an open panel.
window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", themeLabel);
helpBtn.addEventListener("click", showShortcuts);

store.subscribe("streamState", (s) => {
  streamState.dataset.state = s;
  streamState.textContent = s === "live" ? t("stream.live") : s === "reconnecting" ? t("stream.reconnecting") : s === "connecting" ? t("stream.connecting") : "";
});

// ---- routes ----
route("/sites", sitesScreen);
route("/sites/:domain/:tab?", siteScreen);
route("/inbound", inboundScreen);
route("/waf", wafScreen);
route("/modules", modulesScreen);
route("/tasks", tasksScreen);
route("/events", eventsScreen);
route("/guard", guardScreen);
route("/repos", reposScreen);
route("/audit", auditScreen);
route("/edge/:tab?", edgeScreen);
route("/docs/:name?", docsScreen);
route("/settings", settingsScreen);
route("/system", systemScreen);
route("/house", houseScreen);
route("/llm", llmUsageScreen);
// The screen was 'Claude usage' before it held every tool's: old links still land.
route("/claude", llmUsageScreen);
route("/dashboard", dashboardScreen);

// ---- nodes ----
// The selector shows only once another node is linked; until then there
// is one machine and nothing to choose.
async function readNodes() {
  if (delegate()) { store.set("nodes", null); return; }
  try { store.set("nodes", await api.get("/nodes", { node: "" })); } catch { /* an older node, or none: one machine */ }
}
function paintNodes() {
  const n = store.get("nodes");
  const peers = (n && n.members) || [];
  if (currentNode() && n && !peers.some((p) => p.id === currentNode())) setCurrentNode("");
  nodePick.hidden = peers.length === 0;
  clear(nodeSelect);
  if (n) {
    nodeSelect.append(h("option", { value: "" }, t("nodes.thisOne", { name: n.self.name })), ...peers.map((p) => h("option", { value: p.id }, p.name)));
  }
  nodeSelect.value = currentNode();
  nodeSelect.title = t("nodes.pickHint");
  document.getElementById("node-pick-label").textContent = t("nodes.pick");
  app.dataset.remote = currentNode() ? "1" : "";
}
store.subscribe("nodes", paintNodes);
nodeSelect.addEventListener("change", () => {
  // A site's screen belongs to the machine it was opened on.
  if (/^\/sites\/./.test(currentPath())) navigate("/sites");
  setCurrentNode(nodeSelect.value);
});
onNodeChange(() => {
  store.forgetNode();
  paintNodes();
  if (!authed) return;
  disconnect();
  connect();
  readWaiting();
  rerender();
});

let authView = null;
let authed = false;
let mounted = false;

// The skip link targets #main, which the hash router would otherwise read
// as a route; focusing by hand keeps the hash as it is.
document.querySelector(".skip").addEventListener("click", (e) => { e.preventDefault(); main.focus(); });

function showAuth(mode) {
  authed = false;
  disconnect();
  app.dataset.state = "auth";
  sidebar.hidden = true;
  topbar.hidden = false;
  clear(main);
  authView = authScreen({ mode, onDone: boot });
  main.append(authView.el);
  document.title = t("app.title");
}

async function boot() {
  let me;
  try {
    me = await api.get("/auth/me", { allow401: true });
  } catch (e) {
    if (e.status === 401 || e.status === 403) me = { authenticated: false, ...(e.body || {}) };
    else { showAuth("login"); toast(e.message, "bad"); return; }
  }
  store.set("me", me);
  const needsSetup = me && (me.setup_pending || me.needs_setup);
  if (!me || !me.authenticated) { showAuth(needsSetup ? "setup" : "login"); return; }
  authed = true;
  app.dataset.state = "ready";
  document.querySelectorAll(".open-banner").forEach((b) => b.remove());
  if (me.open_until) main.before(openBanner(me.open_until));
  topbar.hidden = false;
  sidebar.hidden = false;
  buildSidebar();
  await readNodes();
  connect();
  readWaiting();
  // A move in course comes back after a reload: its pill, and from it its
  // dialog. Read from the node being looked at, which is the one moving.
  // Moves are the operator's: a delegated user never learns a site has a
  // node to move from.
  if (!pills && !delegate()) {
    pills = movesPill();
    document.body.append(pills.el);
    onNodeChange(() => pills.load());
  }
  if (pills) pills.load();
  if (!mounted) {
    mounted = true;
    // A delegated user's screens are the sites; any other path (a
    // shortcut, an old link) lands on their list.
    mount(main, (path) => (path === "/" || path === "/logout" ? "/sites" : delegate() && !/^\/sites(\/|\?|$)/.test(path) ? "/sites" : path));
  } else {
    rerender();
  }
}

onUnauthorized(() => { if (authed) { showAuth("login"); toast(t("auth.expired"), "bad"); } });
document.addEventListener("route", () => {
  markNav();
  const path = currentPath();
  // A query is part of where the screen is ("#/settings?s=agents"), not
  // another screen: without this the tab of a section read "· middleware".
  const key = NAV.find(([, href]) => path === href || path.startsWith(href + "/") || path.startsWith(href + "?"))?.[0];
  const site = /^\/sites\/([^/?]+)/.exec(path);
  baseTitle = (site ? decodeURIComponent(site[1]) : key ? t("nav." + key) : "") + " · " + t("app.title");
  paintTitle();
});

// Whose turn it is to speak, in the tab's title: with the tab in the
// background the title is all that shows, and a conversation that ended
// asking something waits there for nobody otherwise. Read every half
// minute and when a turn ends; a small answer, cheap to ask often.
//
// Two marks, as in V1: "(2)" is the conversations waiting for an answer,
// "(3…)" the ones talking on their own; waiting wins, it is what someone
// has to do. Counted by the bar's button (liveChats).
let baseTitle = t("app.title");
let titleMarks = { asking: 0, talking: 0 };
function paintTitle() {
  const { asking, talking } = titleMarks;
  document.title = (asking ? `(${asking}) ` : talking ? `(${talking}…) ` : "") + baseTitle;
}
let live = null;
const readWaiting = debounce(async () => {
  // The operator's asks every node and names every site's conversations;
  // a delegate's names their sites' only, and none without a chat to go
  // back to.
  if (!authed) return;
  if (delegate() && !((store.get("me")?.user?.sites) || []).some((s) => (s.perms || []).includes("agents"))) return;
  if (!live) {
    live = liveChats({ onCount: (asking, talking) => { titleMarks = { asking, talking }; paintTitle(); } });
    document.getElementById("live-slot").replaceWith(live.el);
  }
  live.load();
}, 500);
installShortcuts();

(async () => {
  try {
    await loadStrings(getLang());
  } catch (e) {
    main.textContent = "Cannot load the string catalog: " + e.message;
    return;
  }
  themeLabel();
  clear(helpBtn).append(icon("help"));
  helpBtn.setAttribute("aria-label", t("keys.title"));
  menuBtn.setAttribute("aria-label", t("nav.menu"));
  await boot();
})();

// The worker makes the panel installable and is where push lands. Only on
// a secure origin, where browsers allow one; a failure to register is the
// panel without those two things, never the panel not working.
if ("serviceWorker" in navigator && window.isSecureContext) {
  navigator.serviceWorker.register("/sw.js", { scope: "/" }).catch((e) => console.warn("service worker:", e.message));
}

// Exposed for the console: handy when debugging a live installation.
window.mw = { api, store, navigate, GOTO };
