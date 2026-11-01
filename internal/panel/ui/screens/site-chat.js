// Chat with the agent that works inside this site's folder. Turns come
// from GET /chat with their steps (words and tools, in order); while a
// turn runs, the same arrives on the event stream as agent.* events with
// the domain as subject: agent.text, agent.tool when a tool starts,
// agent.tool.result when it answers, agent.subagent when a subagent
// changes state, and the end. What a subagent does goes to its own tab,
// as in V1, out of the way of what the agent itself answers.

import { icon } from "../lib/icons.js";
import { h, clear, debounce } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, apiURL, enc } from "../lib/api.js";
import { store } from "../lib/store.js";
import { can } from "../lib/access.js";
import { onEvent } from "../lib/stream.js";
import { loading, errorBox } from "../components/page.js";
import { confirm, open } from "../components/dialog.js";
import { toast, toastError } from "../components/toast.js";
import { render as renderMd } from "../lib/markdown.js";
import { usageLine } from "../components/usage.js";
import { delegate } from "../lib/access.js";

// chatTab is a site's chat. For the house's own conversation, domain is
// "middleware": no other conversations, no site manifest to write the
// provider into.
export function chatTab(domain) {
  const house = domain === "middleware";
  const turns = h("div", { class: "chat-turns", "aria-live": "polite" });
  const text = h("textarea", { rows: 1, placeholder: t(house ? "chat.placeholderHouse" : "chat.placeholder"), "aria-label": t("chat.message") });
  const provider = h("select", { class: "chat-pill", "aria-label": t("chat.provider"), title: t("chat.provider") });
  // The model: a list while it is short, a box that searches as you type
  // once a provider brings hundreds (OpenCode with OpenRouter connected).
  const modelSel = h("select", { class: "chat-pill", "aria-label": t("chat.model"), title: t("chat.model") });
  const modelListID = "chat-models-" + Math.random().toString(36).slice(2, 8);
  const modelIn = h("input", { type: "text", list: modelListID, class: "model-input chat-pill", placeholder: t("chat.defaultModel"), "aria-label": t("chat.model"), autocomplete: "off", spellcheck: "false", hidden: true });
  const modelList = h("datalist", { id: modelListID });
  let manyModels = false;
  const model = {
    get value() { return manyModels ? modelIn.value.trim() : modelSel.value; },
    set value(v) { modelSel.value = v; modelIn.value = v; },
  };
  // Send and stop are round icon buttons at the end of the box, as in
  // Claude's own app: the arrow goes out, the square stops the turn. While
  // a turn runs the arrow comes back only once something is written, and
  // then it queues the message for when the turn ends.
  const send = h("button", { type: "submit", class: "chat-round send", "aria-label": t("chat.send"), title: t("chat.send") }, icon("arrowUp", { stroke: 2.25 }));
  const stop = h("button", { type: "button", class: "chat-round stop", hidden: true, "aria-label": t("chat.stop"), title: t("chat.stop") }, icon("stop", { stroke: 2.5 }));
  const reset = h("button", { type: "button", class: "btn ghost sm" }, icon("refresh"), t("chat.new"));
  // What the conversation left running, setsid escapes included: listed
  // from the environment mark every turn carries, and killed one by one or
  // all at once. Otherwise they live for days and nobody sees them.
  const procsBtn = h("button", { type: "button", class: "btn ghost sm" }, icon("list"), t("chat.processes"));
  const exportBtn = h("button", { type: "button", class: "btn ghost sm", title: t("chat.exportHint") }, icon("download"), t("chat.export"));
  const status = h("span", { class: "small muted", role: "status" });
  const capacity = h("span", { class: "small muted chat-capacity", title: t("chat.capacityTitle") });
  // Attachments go with the next message; they are kept in the site's own
  // HOME and lent to the turn read-only.
  // The paperclip is a <label> over an input kept off screen, not a
  // button calling input.click(): on a phone a script's click on a hidden
  // file input sometimes opens nothing, and nothing says so (V1).
  const fileID = "chat-file-" + Math.random().toString(36).slice(2, 8);
  const fileInput = h("input", { type: "file", multiple: true, id: fileID, class: "sr-only", tabindex: "-1" });
  const attachBtn = h("label", { for: fileID, class: "chat-round ghost chat-attach", title: t("chat.attachHint"), "aria-label": t("chat.attach") }, icon("plus"));
  const chips = h("div", { class: "chat-chips row" });
  // The usage windows of the agent picked, under the box.
  let usageEl = usageLine(null);
  // The box is one card, as in Claude's own app: what is attached, the
  // words, and under them one row with the paperclip, the agent and its
  // model, and send. The agent and the model used to sit in the bar above
  // with the conversation's picker, four selects in a row that wrapped
  // into three on a phone; here they are where the message is, which is
  // what they apply to.
  const composer = h("div", { class: "chat-composer" }, chips, text,
    h("div", { class: "chat-composer-bar" }, attachBtn, fileInput, provider, modelSel, modelIn, modelList,
      h("span", { class: "grow" }), stop, send));
  const form = h("form", { class: "chat-form" }, composer,
    h("div", { class: "chat-meta" }, status, capacity, usageEl));
  // Which conversation is open: the site's first one, another of the
  // panel's, or one the site's code opened through the site door (read
  // and stopped from here, never written into: a program is on the other
  // side, and its turns follow its own rules).
  const base = `/sites/${enc(domain)}`;
  // Where the conversations beyond the first live: a site's, or the house's.
  const chatsBase = house ? "/chat/middleware" : base;
  const mainConv = () => ({ name: domain, path: house ? "/chat/middleware" : `${base}/chat`, origin: "panel" });
  const convOf = (id, origin) => id ? { name: `${domain}#${id}`, path: `${chatsBase}/chats/${enc(id)}`, origin: origin || "panel" } : mainConv();
  // A new conversation is a draft until its first message (or file) goes:
  // it has no name to list yet, and the title comes from that message.
  const draftConv = () => ({ draft: true, name: null, path: null, origin: "panel" });
  let cur = mainConv();
  const picker = h("select", { class: "chat-title", "aria-label": t("chat.conversation"), title: t("chat.conversation") });
  const newConv = h("button", { type: "button", class: "chat-round ghost", "aria-label": t("chat.newConversation"), title: t("chat.newConversation") }, icon("compose"));
  const purge = h("button", { type: "button", class: "btn ghost sm", title: t("chat.purgeHint") }, icon("trash"), t("chat.purge"));
  const readonlyNote = h("div", { class: "chat-readonly", hidden: true }, icon("lock"), h("span", null, t("chat.siteDoorNote")));
  const idOf = (c) => (c.draft ? "__draft" : c.name === domain ? "" : c.name.split("#")[1]);
  // The list, the last used first; the first time, the last used one is
  // what opens.
  async function fillPicker(first) {
    let list = [];
    try { list = await api.get(`${chatsBase}/chats?main=1`); } catch { list = []; }
    if (first && list.length) cur = convOf(list[0].id, list[0].origin);
    // Opened from the bar's list: that conversation, not the last one.
    const want = store.get("openChat");
    if (first && want && want.domain === domain) {
      store.set("openChat", null);
      const hit = list.find((c) => c.id === want.id);
      if (hit) cur = convOf(hit.id, hit.origin);
    }
    clear(picker);
    const curID = idOf(cur);
    if (!list.some((c) => c.id === curID)) picker.append(h("option", { value: curID, "data-origin": cur.origin }, t("chat.newDraft")));
    for (const c of list) {
      const label = (c.origin === "site" ? t("chat.fromSite") + " · " : "") + (c.title || t("chat.untitled")) + (c.running ? " ●" : "");
      picker.append(h("option", { value: c.id, "data-origin": c.origin, title: c.updated ? new Date(c.updated).toLocaleString() : "" }, label));
    }
    picker.value = curID;
    purge.hidden = list.length === 0;
    applyOrigin();
  }
  function applyOrigin() {
    form.hidden = cur.origin === "site";
    readonlyNote.hidden = cur.origin !== "site";
  }
  picker.addEventListener("change", () => {
    const id = picker.value;
    if (id === "__draft") return;
    cur = convOf(id, picker.selectedOptions[0].dataset.origin);
    applyOrigin();
    loadTurns().then(showUploads);
  });
  newConv.addEventListener("click", async () => {
    if (cur.draft) { focusBox(); return; }
    cur = draftConv();
    await fillPicker();
    loadTurns().then(showUploads);
    focusBox();
  });
  purge.addEventListener("click", async () => {
    if (!(await confirm({ title: t(house ? "chat.purgeQHouse" : "chat.purgeQ", { name: domain }), body: t("chat.purgeBody"), confirmLabel: t("chat.purge"), danger: true }))) return;
    try {
      const r = await api.del(`${chatsBase}/chats`);
      toast(t("chat.purged", { n: (r && r.purged) || 0 }), "ok");
      cur = mainConv();
      await fillPicker();
      loadTurns().then(showUploads);
    } catch (e) { toastError(e); }
  });
  // ensure turns a draft into a conversation, when something is sent.
  // Two quick sends (or a file and a send) on a draft each created a
  // conversation and split the message between them (V1): they share the
  // one being created.
  let creating = null;
  async function ensure() {
    if (!cur.draft) return;
    if (!creating) {
      creating = api.post(`${chatsBase}/chats`, {}).then((r) => { cur = convOf(r.id, "panel"); }).finally(() => { creating = null; });
    }
    await creating;
  }
  // One tab per subagent of the last turn (or opened from an older one),
  // beside the main conversation.
  const tabs = h("div", { class: "chat-tabs", role: "tablist", hidden: true });
  const views = h("div", { class: "chat-views" }, turns);
  // What acts on the open conversation (its processes, starting it over,
  // deleting them all) goes in a menu, not in the bar: used once in a
  // while, it was stealing the room of what is used every message.
  const more = h("details", { class: "chat-menu" },
    h("summary", { class: "chat-round ghost", "aria-label": t("chat.more"), title: t("chat.more") }, icon("more", { stroke: 3 })),
    h("div", { class: "chat-menu-list", role: "menu" }, procsBtn, exportBtn, reset, purge));
  more.addEventListener("click", (e) => { if (e.target.closest(".chat-menu-list button")) more.open = false; });
  const closeMenu = (e) => { if (more.open && !more.contains(e.target)) more.open = false; };
  document.addEventListener("click", closeMenu);
  // Scrolled up to read, a round arrow over the end of the conversation
  // takes the reader back down, as in Claude's app; at the end it hides.
  const toEnd = h("button", { type: "button", class: "chat-round chat-to-end", hidden: true, "aria-label": t("chat.toEnd"), title: t("chat.toEnd") }, icon("arrowDown"));
  const el = h("div", { class: "chat" + (house ? " house" : "") },
    h("div", { class: "chat-head" }, h("div", { class: "chat-title-wrap" }, picker, icon("chevronDown", { class: "chat-title-chev" })), h("div", { class: "chat-head-tools" }, newConv, more)),
    readonlyNote, tabs, h("div", { class: "chat-stage" }, views, toEnd), form);
  let agents = [];
  let running = null; // {turnEl, textEl, textBuf}
  const subs = [() => document.removeEventListener("click", closeMenu)];

  // The "3 of 8 running" indicator is house-wide, not this site's: it is
  // what tells the operator why a send was refused. Events move it at
  // once and a debounced read of /agents/capacity corrects any drift (a
  // turn that ended while the stream was reconnecting).
  const cap = { used: 0, max: 0 };
  function showCapacity() {
    capacity.textContent = cap.max ? t("chat.capacity", { used: cap.used, max: cap.max }) : "";
    capacity.classList.toggle("full", cap.max > 0 && cap.used >= cap.max);
  }
  const syncCapacity = debounce(async () => {
    // The house's capacity names every conversation at work: the
    // operator's to read.
    if (delegate()) return;
    try {
      const c = await api.get("/agents/capacity");
      cap.used = c.used; cap.max = c.max;
      showCapacity();
    } catch { /* the indicator keeps what the events said */ }
  }, 300);
  subs.push(onEvent("agent.", (ev) => {
    const d = ev.data || {};
    if (ev.type === "agent.started") cap.used++;
    else if (ev.type === "agent.done" || (ev.type === "agent.error" && d.turn_id && ev.level === "error")) cap.used = Math.max(0, cap.used - 1);
    else if (ev.type === "agent.capacity") { if (d.used !== undefined) cap.used = d.used; if (d.max !== undefined) cap.max = d.max; }
    else return;
    showCapacity();
    syncCapacity();
  }));

  // --- usage -------------------------------------------------------------

  // What the agent picked has spent of its subscription's windows. Read
  // when the agent changes; told by the bus when a figure moves (Claude
  // says it on every turn's stream); and asked every minute while a turn
  // of this conversation runs, for the agents whose stream says nothing
  // (Codex, Antigravity): a long turn moves the window with nobody typing.
  // The server keeps the vendor's answer a minute, so tabs open on the
  // same agent ask it once.
  function showUsage(st) {
    const n = usageLine(st);
    usageEl.replaceWith(n);
    usageEl = n;
  }
  async function loadUsage() {
    const p = provider.value;
    if (!p || delegate()) { showUsage(null); return; }
    try {
      const list = await api.get(`/agents/usage?provider=${enc(p)}`);
      if (provider.value === p) showUsage((list || []).find((x) => x.provider === p) || null);
    } catch { /* the line keeps what it showed */ }
  }
  let usageTimer = null;
  function followUsage(on) {
    clearInterval(usageTimer);
    usageTimer = on ? setInterval(loadUsage, 60000) : null;
  }
  subs.push(onEvent("agent.usage", (ev) => {
    const d = ev.data || {};
    if (d.provider && d.provider === provider.value && d.usage) showUsage({ provider: d.provider, usage: d.usage });
  }));
  subs.push(() => followUsage(false));

  // --- steps -------------------------------------------------------------

  // What each tool did, said as a person would: "Reads …/src/app.js".
  function toolSummary(name, input) {
    const s = (k) => (input && typeof input[k] === "string" ? input[k] : "");
    const short = (p) => { const parts = p.split("/"); return parts.length <= 3 ? p : "…/" + parts.slice(-2).join("/"); };
    const oneLine = (x, n) => { const l = x.replace(/\s+/g, " ").trim(); return l.length > n ? l.slice(0, n) + "…" : l; };
    switch (name) {
      case "Read": return t("chat.do.read", { path: short(s("file_path")) });
      case "Write": return t("chat.do.write", { path: short(s("file_path")) });
      case "Edit": case "MultiEdit": return t("chat.do.edit", { path: short(s("file_path")) });
      case "NotebookEdit": return t("chat.do.edit", { path: short(s("notebook_path")) });
      case "Bash": return t("chat.do.bash", { cmd: oneLine(s("command"), 120) });
      case "BashOutput": return t("chat.do.bashOutput");
      case "KillShell": case "KillBash": return t("chat.do.kill");
      case "Glob": return t("chat.do.glob", { pattern: s("pattern") });
      case "Grep": return t("chat.do.grep", { pattern: oneLine(s("pattern"), 60) });
      case "WebFetch": return t("chat.do.fetch", { url: s("url") });
      case "WebSearch": return t("chat.do.search", { query: oneLine(s("query"), 60) });
      case "TodoWrite": return t("chat.do.todo");
      case "Task": case "Agent": return t("chat.do.agent", { what: oneLine(s("description"), 60) });
      case "Skill": return t("chat.do.skill", { skill: s("skill") || s("command") });
      default: return t("chat.do.other", { name });
    }
  }

  // The body of a tool's fold: what it was given, the way it reads best,
  // then what it answered.
  function toolBody(tool) {
    const input = tool.input || {};
    const parts = [];
    if (tool.name === "Bash" && input.command) parts.push(h("pre", { class: "cmd" }, "$ " + input.command));
    else if ((tool.name === "Edit") && (input.old_string || input.new_string)) {
      parts.push(h("pre", { class: "diff" },
        ...String(input.old_string || "").split("\n").map((l) => h("span", { class: "del" }, "- " + l + "\n")),
        ...String(input.new_string || "").split("\n").map((l) => h("span", { class: "add" }, "+ " + l + "\n"))));
    } else if (tool.name === "TodoWrite" && Array.isArray(input.todos)) {
      parts.push(h("ul", { class: "todos" }, ...input.todos.map((td) => h("li", { class: td.status }, (td.status === "completed" ? "☑ " : td.status === "in_progress" ? "◐ " : "☐ ") + (td.content || td.activeForm || "")))));
    } else if (tool.name === "Write" && input.content) {
      parts.push(h("pre", null, input.content));
    } else if (Object.keys(input).length && !["Task", "Agent"].includes(tool.name)) {
      parts.push(h("pre", null, JSON.stringify(input, null, 2)));
    }
    const out = h("pre", { class: "out" + (tool.error ? " err" : "") }, tool.output || "");
    out.hidden = !tool.output;
    parts.push(out);
    return { parts, out };
  }

  const RUN_DONE = new Set(["completed", "done"]);
  function runClass(run) {
    if (!run || run === "running") return "live";
    return RUN_DONE.has(run) ? "ok" : "bad";
  }
  function runLabel(run) {
    const k = "chat.run." + (run || "running");
    const s = t(k);
    return s === k ? run : s;
  }

  // toolEls finds a tool's row by its call id, to put its answer on it.
  let toolEls = new Map();
  function toolStepEl(tool, threadData) {
    const state = h("span", { class: "tool-state " + (tool.done ? (tool.error ? "err" : "ok") : "live"), "aria-hidden": "true" });
    const summary = h("summary", null, state, h("span", { class: "tool-what" }, toolSummary(tool.name, tool.input)));
    const body = toolBody(tool);
    const det = h("details", { class: "chat-tool" }, summary, ...body.parts);
    const row = { det, state, out: body.out, tool };
    if (tool.run) {
      const badge = h("span", { class: "badge run " + runClass(tool.run) }, runLabel(tool.run));
      const see = h("button", { type: "button", class: "btn sm ghost" }, t("chat.seeAgent"));
      see.addEventListener("click", (e) => {
        e.preventDefault(); e.stopPropagation();
        agentView(threadData || { id: tool.id, description: (tool.input || {}).description, prompt: (tool.input || {}).prompt, run: tool.run });
        renderTabs();
        showTab(tool.id);
      });
      summary.append(badge, see);
      row.badge = badge;
    }
    if (tool.id) toolEls.set(tool.id, row);
    return det;
  }
  function settleTool(id, d) {
    const row = toolEls.get(id);
    if (!row) return;
    if (d.output !== undefined) { row.out.textContent = d.output; row.out.hidden = !d.output; }
    row.out.classList.toggle("err", Boolean(d.error));
    row.state.className = "tool-state " + (d.error ? "err" : "ok");
    if (d.run && row.badge) { row.badge.className = "badge run " + runClass(d.run); row.badge.textContent = runLabel(d.run); }
    const g = row.det.closest(".chat-steps-group");
    if (g) refreshGroup(g);
  }

  // Tools used one after another are one fold, as in Claude's app: a
  // single quiet line that says what the agent is doing now (or did
  // last) and how many steps it took, open to see each one. Ten rows of
  // monospace between two sentences buried the answer they led to.
  const groups = new WeakMap();
  function groupIn(box) {
    const last = box.lastElementChild;
    if (last && groups.has(last)) return groups.get(last);
    const dot = h("span", { class: "tool-state live", "aria-hidden": "true" });
    const what = h("span", { class: "grp-what" });
    const count = h("span", { class: "grp-count" });
    const list = h("div", { class: "grp-list" });
    const el = h("details", { class: "chat-steps-group" }, h("summary", null, dot, what, count, icon("chevronRight", { class: "grp-chev", size: 16 })), list);
    const g = { el, dot, what, count, list };
    groups.set(el, g);
    box.append(el);
    return g;
  }
  function refreshGroup(el) {
    const g = groups.get(el);
    if (!g) return;
    const rows = [...g.list.children];
    const states = rows.map((r) => r.querySelector(".tool-state"));
    const live = states.some((x) => x && x.classList.contains("live"));
    const bad = states.some((x) => x && x.classList.contains("err"));
    g.dot.className = "tool-state " + (live ? "live" : bad ? "err" : "ok");
    const lastWhat = rows.length ? rows[rows.length - 1].querySelector(".tool-what") : null;
    g.what.textContent = lastWhat ? lastWhat.textContent : "";
    g.count.textContent = rows.length > 1 ? t("chat.stepsN", { n: rows.length }) : "";
  }
  function addTool(box, tool, threadData) {
    const g = groupIn(box);
    const det = toolStepEl(tool, threadData);
    g.list.append(det);
    refreshGroup(g.el);
    return det;
  }

  function textStepEl(text) {
    const md = renderMd(stripQuestions(text || ""), { breaks: true }).el;
    md.classList.add("chat-md");
    // Every code block gets a bar with its language and a copy button,
    // the one thing done with a block more than reading it.
    for (const pre of md.querySelectorAll("pre")) {
      pre.classList.remove("log");
      const bar = h("div", { class: "code-bar" }, h("span", null, pre.dataset.lang || t("chat.code")), copyButton(() => pre.textContent, true));
      const box = h("div", { class: "code-block" });
      pre.replaceWith(box);
      box.append(bar, pre);
    }
    return md;
  }

  // The words of a turn arrive in pieces; each piece drawn as its own
  // markdown broke a list or a code block cut between two of them until
  // the turn was read again. The run of text since the last tool is kept
  // and drawn whole, in the same place, each time a piece comes.
  const liveText = new WeakMap();
  function appendText(box, piece) {
    let cur = liveText.get(box);
    if (!cur || box.lastElementChild !== cur.el) {
      cur = { buf: "", el: null };
    }
    cur.buf += piece;
    const md = textStepEl(cur.buf);
    if (cur.el) cur.el.replaceWith(md); else box.append(md);
    cur.el = md;
    liveText.set(box, cur);
  }

  function stepsEl(steps, threads) {
    const box = h("div", { class: "steps" });
    const byId = new Map((threads || []).map((th) => [th.id, th]));
    for (const st of steps || []) {
      if (st.kind === "text") box.append(textStepEl(st.text));
      else if (st.tool) addTool(box, st.tool, byId.get(st.tool.id));
    }
    return box;
  }

  // --- subagent tabs --------------------------------------------------------

  let agentsUI = new Map();
  let activeTab = "";
  function agentView(th) {
    let a = agentsUI.get(th.id);
    if (a) {
      if (th.description && !a.description) { a.description = th.description; a.title.textContent = th.description; }
      if (th.run) setAgentRun(th.id, th.run);
      return a;
    }
    const title = h("strong", null, th.description || t("chat.subagent"));
    const badge = h("span", { class: "badge run " + runClass(th.run) }, runLabel(th.run));
    const body = h("div", { class: "steps" });
    const foot = h("div", { class: "small muted agent-foot" });
    const view = h("div", { class: "chat-agent", hidden: true }, ...[
      h("div", { class: "row" }, title, badge),
      th.prompt ? h("details", { class: "chat-tool brief" }, h("summary", null, t("chat.brief")), textStepEl(th.prompt)) : null,
      body, foot].filter(Boolean));
    if (th.steps) for (const st of th.steps) { if (st.kind === "text") body.append(textStepEl(st.text)); else if (st.tool) addTool(body, st.tool); }
    views.append(view);
    a = { id: th.id, description: th.description, run: th.run || "running", view, body, badge, title, foot };
    agentsUI.set(th.id, a);
    setAgentRun(th.id, a.run);
    return a;
  }
  function setAgentRun(id, run) {
    const a = agentsUI.get(id);
    if (!a || !run) return;
    a.run = run;
    a.badge.className = "badge run " + runClass(run);
    a.badge.textContent = runLabel(run);
    a.foot.textContent = run === "running" ? t("chat.agentWorking") : t("chat.agentEnded", { state: runLabel(run) });
    a.foot.classList.toggle("typing", run === "running");
    renderTabs();
  }
  function renderTabs() {
    tabs.hidden = agentsUI.size === 0;
    if (tabs.hidden) return;
    const live = [...agentsUI.values()].filter((a) => a.run === "running").length;
    const tab = (id, label, cls) => {
      const b = h("button", { type: "button", role: "tab", class: "chat-tab" + (activeTab === id ? " on" : ""), "aria-selected": String(activeTab === id) }, cls ? h("span", { class: "dot " + cls }) : null, label);
      b.addEventListener("click", () => showTab(id));
      return b;
    };
    clear(tabs).append(...[tab("", t("chat.mainTab")), ...[...agentsUI.values()].map((a) => tab(a.id, a.description || t("chat.subagent"), runClass(a.run))),
      live ? h("span", { class: "small muted live-count" }, t("chat.agentsRunning", { n: live })) : null].filter(Boolean));
  }
  function showTab(id) {
    if (id && !agentsUI.has(id)) id = "";
    activeTab = id;
    turns.hidden = id !== "";
    for (const a of agentsUI.values()) a.view.hidden = a.id !== id;
    renderTabs();
    // Another tab is another length: whether the arrow to the end is
    // wanted is asked again once it is laid out.
    requestAnimationFrame(() => { following = atEnd(); toEnd.hidden = following; });
  }
  function resetAgents() {
    for (const a of agentsUI.values()) a.view.remove();
    agentsUI = new Map();
    toolEls = new Map();
  }

  // A turn as Claude's app draws one: what was written in a soft bubble
  // on the right, the agent's answer with no bubble at all, as prose
  // across the column. Who said it, when and what it cost go in a quiet
  // row under it with a copy button, instead of a header over every turn
  // ("Tú 9:15:02", "Agente 9:15:20 $0.0312") that made the conversation
  // read like a log. who is that row; the name of the speaker stays for
  // screen readers.
  function turnEl(role, body, cost, at, raw) {
    const user = role === "user";
    const words = { text: raw !== undefined ? raw : body || "" };
    const who = h("div", { class: "turn-actions" },
      h("span", { class: "sr-only" }, user ? t("chat.you") : t("chat.agent")),
      copyButton(() => words.text),
      at ? h("time", { class: "turn-time", datetime: at, title: new Date(at).toLocaleString() }, new Date(at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })) : null,
      cost ? h("span", { class: "cost" }, fmtCost(cost)) : null);
    const textEl = user ? h("div", { class: "bubble" }, body || "") : h("div", { class: "steps" });
    if (!user && body) textEl.append(textStepEl(body));
    const el = h("div", { class: "turn " + role }, textEl, who);
    return { el, textEl, who, words };
  }

  // queuedEl is a message waiting for the running turn to end.
  function queuedEl(msg, at) {
    const tu = turnEl("user", msg, undefined, at);
    tu.el.classList.add("is-queued");
    tu.who.append(h("span", { class: "queued" }, t("chat.queued")));
    return tu.el;
  }

  // copyButton copies what text() gives when it is pressed, and says so
  // for a moment. The clipboard API needs a secure page; the panel on a
  // plain loopback address falls back to a selection copied the old way.
  function copyButton(text, labelled) {
    const label = h("span", null, t("chat.copy"));
    const b = h("button", { type: "button", class: "chat-copy" + (labelled ? " labelled" : ""), "aria-label": t("chat.copy"), title: t("chat.copy") }, icon("copy", { size: 15 }), labelled ? label : null);
    b.addEventListener("click", async (e) => {
      e.preventDefault(); e.stopPropagation();
      const s = String(text() || "");
      try {
        if (navigator.clipboard && window.isSecureContext) await navigator.clipboard.writeText(s);
        else {
          const ta = h("textarea", { class: "sr-only", readOnly: true }, s);
          document.body.append(ta);
          ta.select();
          document.execCommand("copy");
          ta.remove();
        }
        b.classList.add("done");
        clear(b).append(icon("check", { size: 15 }), labelled ? h("span", null, t("chat.copied")) : null);
        setTimeout(() => { b.classList.remove("done"); clear(b).append(icon("copy", { size: 15 }), labelled ? label : null); }, 1500);
      } catch { toast(t("chat.copyFailed"), "bad"); }
    });
    return b;
  }

  // A question block is shown as its buttons, not as the JSON the agent
  // wrote; the text around it stays.
  function stripQuestions(s) {
    return s.replace(/^[ \t]*```question[ \t]*\n[\s\S]*?\n[ \t]*```[ \t]*$/gm, "").replace(/\n{3,}/g, "\n\n").trim();
  }

  // The buttons answer by sending the option as the next message; the
  // last one opens the box. Once one is pressed the group is disabled so
  // the same question is not answered twice by accident. No options means
  // the agent wants words, so only the box is offered.
  function questionEl(q) {
    const group = h("div", { class: "chat-question", role: "group", "aria-label": q.question });
    group.append(h("div", { class: "q" }, q.question));
    const row = h("div", { class: "row" });
    const settle = () => { for (const b of row.querySelectorAll("button")) b.disabled = true; };
    for (const opt of q.options || []) {
      const label = opt === q.recommended ? t("chat.recommended", { option: opt }) : opt;
      const b = h("button", { type: "button", class: "btn sm" + (opt === q.recommended ? " primary" : "") }, label);
      b.addEventListener("click", () => { if (running) return; settle(); text.value = opt; form.requestSubmit(); });
      row.append(b);
    }
    const other = h("button", { type: "button", class: "btn sm ghost" }, t("chat.somethingElse"));
    other.addEventListener("click", () => { settle(); text.focus(); });
    row.append(other);
    group.append(row);
    return group;
  }
  function fmtCost(c) {
    const n = Number(c);
    return isNaN(n) ? String(c) : t("chat.cost", { usd: n.toFixed(4) });
  }

  async function loadAgents() {
    try {
      // A delegated user's site may run its turns on another node: the
      // site's own list comes from wherever it is.
      const a = await api.get(delegate() ? `/sites/${enc(domain)}/agents` : "/agents");
      agents = a.providers || [];
      if (a.capacity) { cap.used = a.capacity.used; cap.max = a.capacity.max; showCapacity(); }
    } catch { agents = []; }
    clear(provider);
    for (const a of agents) provider.append(h("option", { value: a.provider, disabled: !a.available }, a.provider + (a.available ? "" : " " + t("chat.unavailable"))));
    if (agents.length === 0) provider.append(h("option", { value: "" }, t("chat.noAgents")));
    fillModels();
  }
  function fillModels() {
    const a = agents.find((x) => x.provider === provider.value);
    const list = (a && a.models) || [];
    manyModels = list.length > 30;
    modelSel.hidden = manyModels;
    modelIn.hidden = !manyModels;
    // The alias alone, so the bar fits a phone; what it runs is its title
    // ("sonnet" runs claude-sonnet-5-5).
    const full = (m) => (a && a.labels && a.labels[m]) || null;
    clear(modelSel).append(h("option", { value: "" }, t("chat.defaultModel")), ...list.map((m) => h("option", { value: m, title: full(m) }, m)));
    clear(modelList).append(...list.map((m) => h("option", { value: m })));
    if (manyModels && !list.includes(modelIn.value.trim())) modelIn.value = "";
  }

  // quiet re-reads without the spinner: after a turn ends, the turn is
  // already on screen and only its final state changes.
  async function loadTurns(quiet) {
    const keepAt = views.scrollTop;
    if (!quiet) clear(turns).append(loading());
    held = [];
    try {
      const conv = cur.draft ? { turns: [] } : await api.get(cur.path);
      clear(turns);
      if (conv.provider && [...provider.options].some((o) => o.value === conv.provider)) provider.value = conv.provider;
      fillModels();
      if (conv.model) model.value = conv.model;
      const list = conv.turns || [];
      const keepTab = activeTab;
      resetAgents();
      list.forEach((tn, i) => {
        const role = tn.role === "user" ? "user" : "agent";
        // Turns from before steps were kept show their words alone.
        const hasSteps = role !== "user" && (tn.steps || []).length;
        const tu = turnEl(role, hasSteps ? "" : tn.text, tn.cost, tn.at, role === "user" ? tn.text : stripQuestions(tn.text || ""));
        if (hasSteps) tu.textEl.replaceWith(stepsEl(tn.steps, tn.threads));
        if (tn.role === "error" && hasSteps) tu.el.insertBefore(h("div", { class: "level-error" }, tn.text.split("\n\n").pop()), tu.who);
        // The last turn's subagents get their tabs at once; older ones open
        // from their row.
        if (role !== "user" && i >= list.length - 2) for (const th of tn.threads || []) if (th.run) agentView(th);
        // Only the last turn's questions are still open; earlier ones
        // were answered by whatever came after them.
        if (i === list.length - 1 && tn.role !== "user" && !conv.running) for (const q of tn.questions || []) tu.el.insertBefore(questionEl(q), tu.who);
        turns.append(tu.el);
      });
      if (!(conv.turns || []).length) turns.append(welcome());
      // A turn still running (it does not depend on this tab: it goes on
      // with the tab closed) is drawn where it is, then followed live.
      running = null;
      seen = { turn: "", seq: 0 };
      showTab(keepTab);
      if (conv.running) {
        begin();
        for (const lv of conv.live || []) handle({ type: lv.type, subject: cur.name, data: lv.data });
        // What was written while it runs, each message in the order it
        // came. The house keeps them, not this tab: drawn only from the
        // reply to the send, they vanished on leaving the page, and a
        // re-read (the tab coming back to the front) wiped them as if the
        // last one had replaced the others.
        for (const q of conv.queued || []) turns.append(queuedEl(q.text, q.at));
      } else {
        followUsage(false);
        syncButtons();
        status.textContent = "";
      }
      loadUsage();
      // A conversation opened lands on its end; a quiet re-read (the end
      // of a turn) keeps the reader where they were.
      if (quiet && !following) views.scrollTop = keepAt;
      else scroll(!quiet);
    } catch (e) {
      clear(turns).append(errorBox(e));
    } finally {
      const late = held || [];
      held = null;
      late.forEach(handle);
    }
  }
  // The chat is a box the height of the screen, as in V1: the bar above
  // and the message box below stay put and only the conversation scrolls
  // (body.chat-fit in app.css). When the whole page scrolled, a long
  // answer took the topbar off the top and the box off the bottom, and on
  // a phone neither came back without scrolling all the way.
  // Following it is keeping its end in view while the reader is there (or
  // has just sent something); scrolled up to read, it stays put.
  let following = true;
  const atEnd = () => views.scrollHeight - views.scrollTop - views.clientHeight < 160;
  views.addEventListener("scroll", () => { following = atEnd(); toEnd.hidden = following; }, { passive: true });
  toEnd.addEventListener("click", () => { views.scrollTo({ top: views.scrollHeight, behavior: "smooth" }); following = true; toEnd.hidden = true; });
  document.body.classList.add("chat-fit");
  subs.push(() => document.body.classList.remove("chat-fit"));
  function scroll(force) {
    if (force) following = true;
    if (following) toEnd.hidden = true;
    if (!following || !el.isConnected) return;
    // After the layout of what was just appended.
    requestAnimationFrame(() => { views.scrollTop = views.scrollHeight; });
  }

  function begin() {
    const tu = turnEl("agent", "");
    // The spark turns while the agent works, as Claude's does; the words
    // shimmer. A blinking "working…" said the same with less to look at.
    const working = h("div", { class: "chat-thinking", role: "status" }, icon("sparkles", { class: "spin", size: 18 }), h("span", { class: "shimmer" }, t("chat.thinking")));
    tu.el.classList.add("running");
    tu.el.insertBefore(working, tu.who);
    turns.append(tu.el);
    running = { ...tu, working };
    followUsage(true);
    syncButtons();
    status.textContent = "";
    scroll();
  }
  function end(ev) {
    if (!running) return;
    running.working.remove();
    running.el.classList.remove("running");
    const cost = ev && ev.data && (ev.data.cost ?? ev.data.cost_usd ?? ev.data.usd);
    if (cost !== undefined) running.who.append(h("span", { class: "cost" }, fmtCost(cost)));
    running = null;
    followUsage(false);
    loadUsage();
    syncButtons();
    status.textContent = "";
  }

  // Events: agent.text (delta), agent.tool / agent.tool.result, agent.cost,
  // agent.end / agent.error / agent.stopped.
  // Events sent while the stream was down are not sent again: on coming
  // back, the conversation is read afresh rather than left showing a
  // turn as running that ended meanwhile.
  let wasLive = store.get("streamState") === "live";
  subs.push(store.subscribe("streamState", (st) => {
    if (st === "live" && !wasLive) loadTurns();
    wasLive = st === "live";
  }));
  // While the conversation is being read, events wait: drawn now they
  // would land in turns that the read is about to replace.
  let held = null;
  subs.push(onEvent("agent.", (ev) => {
    if (ev.subject !== cur.name) return;
    if (held) held.push(ev); else handle(ev);
  }));
  // seen is the last progress event drawn of the running turn: the ones
  // read from the conversation on opening it can arrive again on the
  // stream, and must not be drawn twice.
  let seen = { turn: "", seq: 0 };
  function handle(ev) {
    if (ev.type === "agent.capacity") return;
    // Queued in another tab or device: the conversation says where it
    // goes. This tab's own send draws its bubble itself.
    if (ev.type === "agent.queued") { if (!sending) loadTurns(true); return; }
    const d = ev.data || {};
    if (d.seq) {
      if (d.turn_id === seen.turn && d.seq <= seen.seq) return;
      seen = { turn: d.turn_id, seq: d.seq };
    }
    if (ev.type === "agent.start" || ev.type === "agent.turn.started") { if (!running) begin(); return; }
    // The end of a turn this tab was not drawing (it ended while the
    // conversation was being read): the saved turn says it all.
    if (!running && /^agent\.(done|end|error|stopped|turn\.ended)$/.test(ev.type)) { loadTurns(); return; }
    if (!running) begin();
    // Where it goes: the subagent's tab, or the turn.
    const into = () => (d.thread ? agentView({ id: d.thread }).body : running.textEl);
    if (ev.type === "agent.text" || ev.type === "agent.delta") {
      const piece = d.delta ?? d.text ?? ev.message ?? "";
      if (piece) appendText(into(), piece);
      scroll();
    } else if (ev.type === "agent.tool") {
      const tool = { id: d.id, name: d.tool || d.name || "tool", input: d.input, run: d.run };
      addTool(into(), tool);
      if (d.run) {
        const inp = d.input || {};
        agentView({ id: d.id, description: inp.description, prompt: inp.prompt, run: d.run });
        renderTabs();
      }
      scroll();
    } else if (ev.type === "agent.tool.result") {
      settleTool(d.id, d);
      if (d.run) setAgentRun(d.id, d.run);
    } else if (ev.type === "agent.subagent") {
      settleTool(d.id, { run: d.run });
      setAgentRun(d.id, d.run);
    } else if (ev.type === "agent.question") {
      // After the text that carried it, before whatever the agent says next.
      running.textEl.append(questionEl(d));
      scroll();
    } else if (ev.type === "agent.cost") {
      running.who.append(h("span", { class: "cost" }, fmtCost(d.cost ?? d.usd ?? ev.message)));
    } else if (ev.type === "agent.end" || ev.type === "agent.done" || ev.type === "agent.turn.ended" || ev.type === "agent.stopped") {
      end(ev);
      // The saved turn has every subagent's last state.
      loadTurns(true);
    } else if (ev.type === "agent.error") {
      running.el.insertBefore(h("div", { class: "level-error" }, ev.message || d.error || t("chat.failed")), running.who);
      end(null);
      if (d.turn_id) loadTurns(true);
    }
  }

  // sending holds a second Enter while the first is still on its way;
  // uploading holds the message while a file it should carry is not up.
  let sending = false;
  let uploading = 0;
  function holdSend() { send.disabled = uploading > 0; send.title = uploading > 0 ? t("chat.waitUpload") : running ? t("chat.queue") : t("chat.send"); }
  // While a turn runs the square stops it, and the arrow appears beside
  // it only when there is something to queue; otherwise the arrow alone,
  // dimmed until there is something to send.
  function syncButtons() {
    const written = text.value.trim() !== "";
    stop.hidden = !running;
    send.hidden = Boolean(running) && !written;
    send.classList.toggle("empty", !written);
    const label = running ? t("chat.queue") : t("chat.send");
    send.setAttribute("aria-label", label);
    if (!uploading) send.title = label;
  }
  // An empty conversation opens on a greeting and a few things to ask,
  // as Claude's app does, instead of a line saying there is nothing.
  function welcome() {
    const keys = house ? ["chat.suggest.house1", "chat.suggest.house2", "chat.suggest.house3"] : ["chat.suggest.site1", "chat.suggest.site2", "chat.suggest.site3"];
    const chips = keys.map((k) => {
      const b = h("button", { type: "button", class: "chat-suggestion" }, t(k));
      b.addEventListener("click", () => { text.value = t(k); fit(); syncButtons(); text.focus(); });
      return b;
    });
    return h("div", { class: "chat-welcome" },
      icon("sparkles", { size: 30, class: "chat-welcome-mark" }),
      h("h2", null, house ? t("chat.helloHouse") : t("chat.hello", { name: domain })),
      h("p", null, t(house ? "chat.emptyHintHouse" : "chat.emptyHint")),
      cur.origin === "site" ? null : h("div", { class: "chat-suggestions" }, ...chips));
  }
  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    const msg = text.value.trim();
    if (!msg || sending || uploading > 0) return;
    sending = true;
    const whileRunning = Boolean(running);
    turns.querySelector(".chat-welcome")?.remove();
    turns.querySelector(".chat-notice")?.remove();
    const mine = turnEl("user", msg).el;
    // What is written goes to the main conversation: shown there, not
    // under a subagent's tab that does not hear it.
    if (activeTab) showTab("");
    turns.append(mine);
    scroll(true);
    text.value = "";
    fit();
    syncButtons();
    send.classList.add("busy");
    // Gives the message back when it did not go: the bubble stays, dimmed,
    // and the words return to the box unless something new was typed
    // there meanwhile. Losing a paragraph because the network blinked is
    // what makes people write somewhere else and paste (V1).
    const giveBack = (why) => {
      mine.classList.add("failed");
      mine.append(h("div", { class: "level-error small" }, why));
      if (!text.value.trim()) { text.value = msg; fit(); }
    };
    try {
      const s = store.site(domain);
      const agentCfg = (s && s.manifest && s.manifest.agent) || {};
      // Remembered as the site's default where its manifest may be
      // written; the message carries the choice either way.
      if (!house && can(domain, "code") && provider.value && provider.value !== agentCfg.provider) {
        await api.patch(`/sites/${enc(domain)}/manifest`, { agent: { ...agentCfg, provider: provider.value, model: model.value } });
      }
      await ensure();
      const r = await api.post(cur.path, { text: msg, model: model.value || undefined, provider: provider.value || undefined });
      // The title comes from this message: the list shows it now.
      fillPicker();
      if (r && r.queued) {
        // Kept for when the running turn ends; it goes out with any other
        // queued message as the next one.
        mine.querySelector(".turn-actions").append(h("span", { class: "queued" }, t("chat.queued")));
      } else if (!running && !whileRunning) begin();
    } catch (err) {
      if (err.status === 429) {
        // The house is full: the message goes back in the box, whole, and
        // the reason is said where the operator is looking.
        mine.remove();
        if (!text.value.trim()) { text.value = msg; fit(); }
        turns.append(h("div", { class: "notice warn chat-notice" }, t("chat.capacityFull", { used: cap.used, max: cap.max })));
        scroll();
        syncCapacity();
        end(null);
        return;
      }
      giveBack(err.message || t("chat.failed"));
      if (!whileRunning) end(null);
    } finally {
      sending = false;
      send.classList.remove("busy");
    }
  });
  // Enter sends and Shift+Enter is a new line, at a keyboard. On a touch
  // screen Enter is a new line: its keyboard has no Shift+Enter, so it is
  // the only way to write two paragraphs, and a stray Enter would send
  // half a message (V1). Ctrl/Cmd+Enter sends everywhere.
  const touch = window.matchMedia("(hover: none) and (pointer: coarse)");
  text.addEventListener("keydown", (e) => {
    if (e.key !== "Enter" || e.isComposing) return;
    if (e.metaKey || e.ctrlKey || (!e.shiftKey && !e.altKey && !touch.matches)) { e.preventDefault(); form.requestSubmit(); }
  });
  // The box grows with what is written, up to a point, and goes back to
  // one line once sent.
  function fit() {
    text.style.height = "auto";
    text.style.height = Math.min(text.scrollHeight + 2, 192) + "px";
    // A taller box leaves less of the conversation: its end stays in view.
    scroll();
  }
  text.addEventListener("input", () => { fit(); syncButtons(); });
  // Focus on deliberate gestures only, and never on a touch screen: there
  // it opens the keyboard over half the conversation (V1).
  function focusBox() { if (!touch.matches) text.focus(); }
  provider.addEventListener("change", () => { fillModels(); loadUsage(); });
  stop.addEventListener("click", async () => {
    try { await api.post(`${cur.path}/stop`); status.textContent = t("chat.stopping"); } catch (err) { toastError(err); }
  });
  // A plain link, so the browser saves it with the session cookie: the
  // whole conversation with every step, for someone else to read.
  exportBtn.addEventListener("click", () => {
    if (!cur.path) { toast(t("chat.exportEmpty")); return; }
    const a = h("a", { href: apiURL(cur.path + "/export"), download: "" });
    document.body.append(a);
    a.click();
    a.remove();
  });
  procsBtn.addEventListener("click", async () => {
    const list = h("div", { class: "stack-sm" });
    const fill = async () => {
      clear(list);
      const procs = await api.get(`${cur.path}/processes`);
      if (!procs.length) { list.append(h("p", { class: "muted" }, t("chat.noProcesses"))); return; }
      for (const pr of procs) {
        const kill = h("button", { type: "button", class: "btn sm danger" }, t("chat.kill"));
        kill.addEventListener("click", async () => {
          try { await api.post(`${cur.path}/processes/${pr.pid}/kill`); await fill(); } catch (err) { toastError(err); }
        });
        list.append(h("div", { class: "row" }, h("code", { class: "grow small" }, `${pr.pid}  ${pr.command}`), kill));
      }
    };
    try { await fill(); } catch (err) { toastError(err); return; }
    await open({
      title: t("chat.processesTitle"),
      body: [h("p", { class: "dim" }, t("chat.processesBody")), list],
      buttons: [{ label: t("common.close"), value: null }, { label: t("chat.killAll"), primary: true, onClick: async () => {
        const r = await api.post(`${cur.path}/reap`);
        toast(t("chat.killed", { n: r.killed }), "ok");
        await fill();
        return false;
      } }],
    });
  });
  reset.addEventListener("click", async () => {
    if (!(await confirm({ title: t("chat.newQ"), body: t("chat.newBody"), confirmLabel: t("chat.new") }))) return;
    try { await api.del(cur.path); await loadTurns(); } catch (err) { toastError(err); }
  });

  async function showUploads() {
    clear(chips);
    let ups = [];
    if (cur.draft) return;
    try { ups = await api.get(`${cur.path}/files`); } catch { ups = []; }
    for (const u of ups) {
      const x = h("button", { type: "button", class: "btn sm ghost", "aria-label": t("chat.removeFile", { name: u.name }) }, "×");
      x.addEventListener("click", async () => { try { await api.del(`${cur.path}/files/${enc(u.name)}`); showUploads(); } catch (e) { toastError(e); } });
      chips.append(h("span", { class: "badge" }, u.name, " ", x));
    }
  }
  // One at a time, with a chip that says it is on its way: in parallel
  // every upload crawled at a fraction and none of the bars meant anything
  // (V1). Send waits until they are all up.
  async function upload(files) {
    if (!files.length) return;
    uploading++;
    holdSend();
    try {
      await ensure();
      for (const f of files) {
        const chip = h("span", { class: "badge chat-uploading" }, f.name, " ", h("span", { class: "typing" }));
        chips.append(chip);
        try { await api.postRaw(`${cur.path}/files?name=${enc(f.name)}`, f, f.type || "application/octet-stream"); }
        catch (e) { toastError(e); }
        chip.remove();
      }
    } catch (e) { toastError(e); } finally {
      uploading--;
      holdSend();
      showUploads();
    }
  }
  fileInput.addEventListener("change", () => { upload([...fileInput.files]); fileInput.value = ""; });
  // A screenshot is sent the natural way: Print Screen, then paste (V1).
  text.addEventListener("paste", (e) => {
    const files = [...((e.clipboardData && e.clipboardData.files) || [])];
    if (!files.length) return;
    e.preventDefault();
    upload(files.map((f, i) => (f.name && f.name !== "image.png" ? f : new File([f], `paste-${Date.now()}-${i}.${(f.type.split("/")[1] || "png")}`, { type: f.type }))));
  });
  // The whole chat takes a drop, not just the box: whoever drags is aiming
  // at the conversation. Only a drag that carries files is answered, and
  // it is outlined while it hovers.
  const carriesFiles = (e) => e.dataTransfer && [...e.dataTransfer.types].includes("Files");
  let dragDepth = 0;
  el.addEventListener("dragenter", (e) => { if (!carriesFiles(e) || cur.origin === "site") return; dragDepth++; el.classList.add("dropping"); });
  el.addEventListener("dragleave", () => { if (dragDepth && --dragDepth === 0) el.classList.remove("dropping"); });
  el.addEventListener("dragover", (e) => { if (carriesFiles(e)) e.preventDefault(); });
  el.addEventListener("drop", (e) => {
    dragDepth = 0;
    el.classList.remove("dropping");
    if (!carriesFiles(e)) return;
    e.preventDefault();
    if (cur.origin !== "site") upload([...e.dataTransfer.files]);
  });
  // A phone that slept can leave the stream quiet without saying so: back
  // on screen, the conversation is read again (V1).
  const onVisible = () => { if (document.visibilityState === "visible" && !held) loadTurns(true); };
  document.addEventListener("visibilitychange", onVisible);
  subs.push(() => document.removeEventListener("visibilitychange", onVisible));
  subs.push(onEvent("agent.started", (ev) => { if (ev.subject === cur.name) showUploads(); }));
  // The running mark beside each conversation follows the turns.
  subs.push(onEvent("agent.", (ev) => { if (/^agent\.(started|done|error)$/.test(ev.type) && ev.subject && ev.subject.split("#")[0] === domain) fillPicker(); }));

  loadAgents().then(() => fillPicker(true)).then(() => loadTurns()).then(showUploads);
  return { el, destroy() { subs.forEach((u) => u()); } };
}
