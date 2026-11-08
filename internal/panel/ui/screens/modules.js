// Modules: one line each, grouped by what they are for, and opened on
// demand into a form built from what the module says it needs, with
// Verify / Enable that show the result lines as a checklist.
// Secrets are password fields and are never echoed back: the API masks
// them, so an empty secret field on save means "keep what is stored".

import { h, clear, copy, fmtTime } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { onEvent } from "../lib/stream.js";
import { page, load, empty, notice } from "../components/page.js";
import { icon } from "../lib/icons.js";
import { openCodeProviders, vendorModels } from "./opencode-providers.js";
import { usageBlock } from "../components/usage.js";

// The vendors' own modules: a key and their models, run by OpenCode.
const LOGIN_VENDORS = new Set(["agent-openai"]);
const VENDORS = new Set(["agent-openai", "agent-deepseek", "agent-zai", "agent-kimi", "agent-qwen", "agent-mistral", "agent-xai"]);

import { checklist } from "../components/checklist.js";
import { confirm, field, open as openDialog, checkbox } from "../components/dialog.js";
import { currentNode } from "../lib/node.js";
import { toast, toastError } from "../components/toast.js";

export function modulesScreen() {
  // The programs the modules run are checked against their registries
  // once a day; this asks now, for whoever just read of a release.
  const checkBtn = h("button", { type: "button", class: "btn" }, icon("refresh"), t("modules.depsCheck"));
  const p = page({ title: t("nav.modules"), sub: t("modules.sub"), icon: "blocks", actions: checkBtn });
  checkBtn.addEventListener("click", async () => {
    checkBtn.classList.add("busy");
    try { await api.post("/deps/check"); toast(t("modules.depsChecked"), "ok"); load(body, render); }
    catch (e) { toastError(e); } finally { checkBtn.classList.remove("busy"); }
  });
  const body = h("div", { class: "module-groups" });
  p.body.append(body);
  const subs = [];

  async function render() {
    const [mods, deps, net] = await Promise.all([api.get("/modules"), api.get("/deps").catch(() => []), api.get("/nodes").catch(() => null)]);
    // The other nodes of the network, seen from the node these modules
    // are on: /nodes is always this panel's own, and the panel may be
    // working on another node, which is then the one left out.
    others = [];
    if (net && net.network && net.self) {
      const from = currentNode() || net.self.id;
      others = [net.self, ...(net.members || [])].filter((x) => x.id !== from);
    }
    clear(body);
    if (!mods || mods.length === 0) { body.append(empty(t("modules.emptyTitle"), t("modules.emptyHint"))); return null; }
    const depBy = new Map((deps || []).map((d) => [d.name, d]));
    // Grouped by what they are for, in a fixed order; one the panel does
    // not know yet goes under "others" rather than nowhere.
    const groups = new Map(GROUPS.map((g) => [g, []]));
    for (const m of mods) groups.get(GROUP_OF[m.key] || "other").push(m);
    for (const [g, list] of groups) {
      if (!list.length) continue;
      const on = list.filter((m) => m.enabled).length;
      body.append(h("section", { class: "module-group" },
        h("div", { class: "mod-group-head" }, h("h3", null, t("modules.group." + g)),
          h("span", { class: "mod-group-count" }, t("modules.groupOn", { n: on, total: list.length }))),
        h("div", { class: "card pad0 mod-list" }, ...list.map((m) => row(m, depBy)))));
    }
    return null;
  }

  let others = [];

  // The modules opened, by key: a module event draws the list again, and
  // the one being set up must still be open after it.
  const opened = new Set();

  // A module is one line: what it is, whether it is on, and the one thing
  // to do (set it up, or manage it). Every card used to carry its whole
  // form, credentials and all, enabled or not: the page was 8,000px on a
  // phone for the two modules anyone was there for. The form is built
  // the first time the line is opened, and the sign-in polls with it.
  function row(m, depBy) {
    const id = "mod-body-" + m.key;
    const panel = h("div", { class: "mod-body", id, hidden: true });
    const stateEl = h("span", { class: "mod-state" });
    const go = h("span", { class: "mod-go" });
    const head = h("button", { type: "button", class: "mod-head", "aria-expanded": "false", "aria-controls": id },
      h("span", { class: "module-icon" }, icon(ICON_OF[m.key] || "blocks")),
      h("span", { class: "mod-text" },
        h("span", { class: "mod-name" }, m.name || m.key),
        h("span", { class: "mod-desc" }, said("modules.desc." + m.key, m.description || ""))),
      stateEl, go, h("span", { class: "mod-chev", "aria-hidden": "true" }));
    const el = h("div", { class: "mod-row" }, head, panel);
    const paintState = () => {
      el.classList.toggle("on", Boolean(m.enabled));
      clear(stateEl).append(stateBadge(m));
      const bin = AGENT_BIN[m.key] || TOOL_BIN[m.key];
      const d = bin && depBy.get(bin);
      if (m.enabled && d && d.outdated) stateEl.append(h("span", { class: "badge warn" }, t("modules.updateBadge")));
      go.textContent = m.enabled ? t("modules.manage") : t("modules.setUp");
      go.classList.toggle("primary", !m.enabled);
    };
    paintState();
    let built = false;
    const setOpen = (open) => {
      if (open && !built) { panel.append(...card(m, depBy, paintState)); built = true; }
      panel.hidden = !open;
      head.setAttribute("aria-expanded", String(open));
      el.classList.toggle("open", open);
      if (open) opened.add(m.key); else opened.delete(m.key);
    };
    head.addEventListener("click", () => setOpen(panel.hidden));
    if (opened.has(m.key)) setOpen(true);
    return el;
  }

  const GROUPS = ["connect", "deploy", "agents", "work", "other"];
  const GROUP_OF = { cloudflare: "connect", vpn: "connect", github: "deploy", openspec: "work", "agent-claude": "agents", "agent-agy": "agents", "agent-opencode": "agents", "agent-openai": "agents", "agent-deepseek": "agents", "agent-zai": "agents", "agent-kimi": "agents", "agent-qwen": "agents", "agent-mistral": "agents", "agent-xai": "agents" };
  const ICON_OF = { cloudflare: "globe", vpn: "lock", github: "branch", openspec: "file", "agent-claude": "sparkles", "agent-agy": "sparkles", "agent-opencode": "sparkles", "agent-openai": "sparkles", "agent-deepseek": "sparkles", "agent-zai": "sparkles", "agent-kimi": "sparkles", "agent-qwen": "sparkles", "agent-mistral": "sparkles", "agent-xai": "sparkles" };
  // The panel's own words for a module when it has them; the module's
  // (in English, from the server) otherwise.
  const said = (key, fallback) => { const s = t(key); return s === key ? fallback : s; };

  // Which command an agent module runs, for the binary line.
  // A tool's command, for the same line (no sign-in: it is not an agent).
  const TOOL_BIN = { openspec: "openspec" };
  const AGENT_BIN = { "agent-claude": "claude", "agent-agy": "agy", "agent-opencode": "opencode", "agent-openai": "codex", "agent-deepseek": "claude", "agent-zai": "claude", "agent-kimi": "kimi", "agent-qwen": "qwen", "agent-mistral": "vibe", "agent-xai": "grok" };

  // What an opened module shows: its sign-in, its program, its models and
  // its form. paintState redraws the line above once it is enabled.
  function card(m, depBy, paintState) {
    const inputs = {};
    const form = h("form", { novalidate: true, class: "needs" });
    // A module that needs nothing typed (every field has a default the
    // module fills, as the VPN) is a switch: on or off, its fields folded
    // under "advanced" for whoever wants to change them.
    const visible = (m.needs || []).filter((n) => !n.hidden);
    const simple = visible.length > 0 && visible.every((n) => n.optional);
    // An enabled module's settings are folded: the card says what it is
    // and that it works; the fields are there for whoever opens them.
    const folded = simple || (m.enabled && visible.length > 0);
    const fields = folded ? h("details", { class: "advanced" }, h("summary", null, t(simple ? "modules.advanced" : "modules.settings"))) : form;
    for (const n of m.needs || []) {
      // A hidden need is filled by the module itself (a session from a
      // sign-in); it has its own place on the card.
      if (n.hidden) continue;
      const input = n.options && n.options.length ? choices(n) : h("input", {
        type: n.secret ? "password" : "text", autocomplete: n.secret ? "new-password" : "off",
        class: "mono", placeholder: n.secret && m.enabled ? t("modules.secretKept") : "", value: n.value || "",
      });
      inputs[n.key] = input;
      fields.append(field(said(`modules.need.${m.key}.${n.key}`, n.label || n.key), input, said(`modules.needHelp.${m.key}.${n.key}`, n.help || n.hint)));
    }
    if (folded) form.append(fields);
    const lines = checklist();
    const verifyBtn = h("button", { type: "button", class: "btn", hidden: simple || !(m.needs && m.needs.length) }, t("modules.verify"));
    const enableBtn = h("button", { type: "submit", class: "btn primary", hidden: simple && m.enabled }, m.enabled ? t("modules.save") : t("modules.enable"));
    const disableBtn = h("button", { type: "button", class: "btn" + (simple ? " danger" : ""), hidden: !m.enabled }, t("modules.disable"));
    // Forgetting the credentials is the rare and the destructive one: a
    // quiet red word at the far end of the row, not a third button.
    const forgetBtn = h("button", { type: "button", class: "btn ghost sm danger mod-forget", hidden: simple || !(m.needs && m.needs.length) }, t("modules.forget"));
    // On a switch, changing an advanced field and saving is the only reason
    // to press "save" while it is on.
    if (simple) {
      const saveAdvanced = h("button", { type: "submit", class: "btn sm", hidden: !m.enabled }, t("modules.save"));
      fields.append(h("div", { class: "form-actions" }, saveAdvanced));
    }
    const config = () => {
      const c = {};
      for (const [k, input] of Object.entries(inputs)) {
        // A secret is never shown, so empty means "keep it"; any other
        // field shows its value, and emptied is a choice ("the first one
        // listed") the server clears.
        if (input.value !== "" || input.type !== "password") c[k] = input.value;
      }
      return c;
    };
    const run = async (btn, path, done) => {
      btn.classList.add("busy");
      lines.set([{ state: "pending", text: t("modules.running") }]);
      try {
        const r = await api.post(path, { config: config() });
        lines.set(r && r.lines);
        if (r && r.ok === false) toast(t("modules.failed", { name: m.name }), "bad");
        else if (done) { toast(done, "ok"); }
        return r;
      } catch (e) {
        lines.set([{ ok: false, text: e.message }]);
      } finally { btn.classList.remove("busy"); }
      return null;
    };
    verifyBtn.addEventListener("click", () => run(verifyBtn, `/modules/${enc(m.key)}/verify`, t("modules.verified", { name: m.name })));
    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      const r = await run(enableBtn, `/modules/${enc(m.key)}/enable`, t("modules.enabled", { name: m.name }));
      if (r && r.ok !== false) {
        m.enabled = true; paintState(); enableBtn.textContent = t("modules.save"); disableBtn.hidden = false;
        // A vendor's card shows its models once it is on: drawn again.
        if (simple || VENDORS.has(m.key)) load(body, render);
      }
    });
    disableBtn.addEventListener("click", async () => {
      if (!(await confirm({ title: t("modules.disableQ", { name: m.name }), body: t("modules.disableBody"), confirmLabel: t("modules.disable"), danger: true }))) return;
      try { await api.post(`/modules/${enc(m.key)}/disable`); toast(t("modules.disabled", { name: m.name }), "ok"); load(body, render); } catch (err) { toastError(err); }
    });
    forgetBtn.addEventListener("click", async () => {
      if (!(await confirm({ title: t("modules.forgetQ", { name: m.name }), body: t("modules.forgetBody"), confirmLabel: t("modules.forget"), danger: true }))) return;
      try { await api.del(`/modules/${enc(m.key)}/credentials`); toast(t("modules.forgotten", { name: m.name }), "ok"); load(body, render); } catch (err) { toastError(err); }
    });
    // Carrying the credentials to the other nodes, so each is not set up
    // by hand: every node is ticked, the operator unticks the ones that
    // should not have them. Each takes them through its own verify and
    // install, so a node that refuses them says why here.
    const propagateBtn = h("button", { type: "button", class: "btn", hidden: !(m.shareable && others.length) }, icon("server"), t("modules.propagate"));
    propagateBtn.addEventListener("click", () => propagate(m, lines));
    form.append(lines.el, h("div", { class: "form-actions" }, enableBtn, verifyBtn, propagateBtn, disableBtn, forgetBtn));
    const agentBin = AGENT_BIN[m.key];
    return [
      m.installed === false && m.enabled && !agentBin ? notice("warn", t("modules.notInstalled")) : null,
      // OpenCode has no sign-in of its own: a key per provider instead.
      m.key === "agent-opencode" ? openCodeProviders() : null,
      // A vendor with a subscription sign-in (OpenAI's Codex) offers it,
      // beside its models.
      LOGIN_VENDORS.has(m.key) || (!VENDORS.has(m.key) && agentBin && m.key !== "agent-opencode") ? signIn(m) : null,
      VENDORS.has(m.key) && m.enabled ? vendorModels(m.key) : null,
      agentBin ? binaryLine(agentBin, depBy.get(agentBin)) : null,
      TOOL_BIN[m.key] && m.enabled ? binaryLine(TOOL_BIN[m.key], depBy.get(TOOL_BIN[m.key])) : null,
      form,
    ].filter(Boolean); // a native append writes null as the text "null"
  }

  async function propagate(m, lines) {
    const boxes = others.map((x) => ({ x, ...checkbox(x.name || x.id, { checked: true }) }));
    const all = checkbox(t("modules.propagateAll"), { checked: true });
    all.input.addEventListener("change", () => boxes.forEach((b) => { b.input.checked = all.input.checked; }));
    boxes.forEach((b) => b.input.addEventListener("change", () => { all.input.checked = boxes.every((c) => c.input.checked); }));
    const r = await openDialog({
      title: t("modules.propagateQ", { name: m.name }),
      body: [h("p", null, t("modules.propagateBody")), h("div", { class: "stack-sm" }, all.el, ...boxes.map((b) => b.el))],
      buttons: [
        { label: t("common.cancel"), value: null },
        { label: t("modules.propagate"), primary: true, onClick: async (_form, showError) => {
          const chosen = boxes.filter((b) => b.input.checked).map((b) => b.x.id);
          if (!chosen.length) { showError(t("modules.propagateNone")); return false; }
          return api.post(`/modules/${enc(m.key)}/propagate`, { nodes: chosen });
        } },
      ],
    });
    if (!r || !r.results) return;
    // One line per node, and under a node that refused, its own lines.
    const out = [];
    for (const n of r.results) {
      out.push({ ok: n.ok, text: n.ok ? t("modules.propagatedTo", { node: n.name }) : t("modules.propagateFailed", { node: n.name, why: n.error || "" }) });
      if (!n.ok) for (const l of n.lines || []) if (l.status === "fail") out.push({ status: "fail", text: `${n.name}: ${l.text}` });
    }
    lines.set(out);
    toast(r.ok ? t("modules.propagated", { name: m.name }) : t("modules.propagatePartial", { name: m.name }), r.ok ? "ok" : "bad");
  }

  // The sign-in with a subscription: the vendor's own flow, run on a
  // terminal of the server's. Start → open the link on any device →
  // paste the code → the session is stored and the module enabled. The
  // state is polled every two seconds and never waited on in a request.
  function signIn(m) {
    const box = h("div", { class: "signin" });
    const sessionLine = h("p", { class: "small" });
    const startBtn = h("button", { type: "button", class: "btn primary sm" }, icon("key"), t("modules.signIn"));
    const flow = h("div", { hidden: true });
    let timer = null;
    const stop = () => { if (timer) { clearInterval(timer); timer = null; } };
    const showSession = (s) => {
      clear(sessionLine);
      if (!s || s.kind === "none") sessionLine.append(h("span", { class: "muted" }, t("modules.sessionNone")));
      else if (s.kind === "api_key") sessionLine.append(h("span", { class: "badge info" }, t("modules.sessionKey")));
      else sessionLine.append(h("span", { class: "badge ok" }, t("modules.sessionSubscription")), " ", h("span", { class: "muted" }, s.since ? t("modules.sessionSince", { when: fmtTime(s.since) }) : ""));
      showUsage(s && s.kind === "subscription");
    };
    // How much of the subscription is spent, under the session: asked of
    // the vendor (cached a minute there) and, failing that, what the last
    // turn said.
    const usageBox = h("div", { class: "module-usage", hidden: true });
    const showUsage = async (on, refresh) => {
      usageBox.hidden = !on;
      if (!on) return;
      const again = h("button", { type: "button", class: "btn ghost sm" }, icon("refresh"), t("usage.refresh"));
      again.addEventListener("click", () => { again.classList.add("busy"); showUsage(true, true); });
      try {
        const st = await api.get(`/modules/${enc(m.key)}/usage` + (refresh ? "?refresh=1" : ""));
        clear(usageBox).append(h("div", { class: "log-bar" }, h("span", { class: "label" }, t("usage.title")), h("span", { class: "grow" }), again), usageBlock(st));
      } catch (e) { clear(usageBox).append(h("p", { class: "small muted" }, e.message)); }
    };
    const render = (st) => {
      clear(flow);
      flow.hidden = false;
      if (st.error && st.state !== "authorize") flow.append(notice("bad", st.error));
      if (st.state === "starting") flow.append(h("p", { class: "small muted" }, t("modules.signInStarting")));
      if (st.state === "authorize" && st.device_code) {
        // A device sign-in (Codex with ChatGPT): a link and a code to type
        // there; nothing comes back here, the program notices by itself.
        const copyCode = h("button", { type: "button", class: "btn sm" }, icon("copy"), t("common.copy"));
        copyCode.addEventListener("click", async () => { toast((await copy(st.device_code)) ? t("common.copied") : st.device_code, "ok"); });
        flow.append(h("ol", { class: "steps small" },
          h("li", null, t("modules.deviceStep1"), " ", h("a", { href: st.url, target: "_blank", rel: "noopener" }, icon("external", { size: 14 }), st.url)),
          h("li", null, t("modules.deviceStep2"), " ", h("code", { class: "device-code" }, st.device_code), " ", copyCode),
          h("li", null, t("modules.deviceStep3"))));
      }
      if (st.state === "authorize" && !st.device_code) {
        const code = h("input", { type: "text", class: "mono", placeholder: t("modules.signInCodePlaceholder"), autocomplete: "off", spellcheck: "false" });
        const send = h("button", { type: "button", class: "btn primary sm" }, icon("send"), t("modules.signInSend"));
        const copyBtn = h("button", { type: "button", class: "btn sm" }, icon("copy"), t("common.copy"));
        copyBtn.addEventListener("click", async () => { toast((await copy(st.url)) ? t("common.copied") : st.url, "ok"); });
        send.addEventListener("click", async () => {
          send.classList.add("busy");
          try { render(await api.post(`/modules/${enc(m.key)}/login/code`, { code: code.value })); }
          catch (e) { toastError(e); } finally { send.classList.remove("busy"); }
        });
        flow.append(
          h("ol", { class: "steps small" },
            h("li", null, t("modules.signInStep1"), " ", h("a", { href: st.url, target: "_blank", rel: "noopener" }, icon("external", { size: 14 }), t("modules.signInOpen")), " ", copyBtn),
            h("li", null, t("modules.signInStep2")),
          ),
          h("div", { class: "row" }, code, send),
        );
        // A native append() would print "null" for a missing error.
        if (st.error) flow.append(notice("warn", st.error));
        setTimeout(() => code.focus(), 0);
      }
      if (st.state === "checking") flow.append(h("p", { class: "small muted" }, t("modules.signInChecking")));
      if (st.tail && st.state !== "authorize") flow.append(h("pre", { class: "log small" }, st.tail));
      const cancel = h("button", { type: "button", class: "btn ghost sm" }, icon("x"), t("common.cancel"));
      cancel.addEventListener("click", async () => { stop(); try { await api.del(`/modules/${enc(m.key)}/login`); } catch { /* gone already */ } flow.hidden = true; startBtn.hidden = false; });
      if (st.state === "starting" || st.state === "authorize" || st.state === "checking") flow.append(h("div", { class: "actions" }, cancel));
      else { startBtn.hidden = false; stop(); }
    };
    const poll = async () => {
      try {
        const st = await api.get(`/modules/${enc(m.key)}/login`);
        if (st.state === "done") {
          stop();
          flow.hidden = true;
          startBtn.hidden = false;
          showSession(st.session);
          toast(t("modules.signedIn", { name: m.name }), "ok");
          load(body, render0);
          return;
        }
        if (st.state === "failed") { stop(); render(st); return; }
        if (st.state) render(st);
      } catch (e) { stop(); render({ state: "failed", error: e.message }); }
    };
    startBtn.addEventListener("click", async () => {
      startBtn.hidden = true;
      render({ state: "starting" });
      try {
        render(await api.post(`/modules/${enc(m.key)}/login`, {}));
        stop();
        timer = setInterval(poll, 2000);
      } catch (e) { render({ state: "failed", error: e.message }); }
    });
    api.get(`/modules/${enc(m.key)}/session`).then((s) => showSession(s.session)).catch(() => showSession(null));
    // A sign-in left open in another tab is picked up here.
    api.get(`/modules/${enc(m.key)}/login`).then((st) => { if (st.state && st.state !== "done" && st.state !== "failed") { startBtn.hidden = true; render(st); timer = setInterval(poll, 2000); } }).catch(() => {});
    box.append(h("div", { class: "log-bar" }, h("span", { class: "label" }, t("modules.session")), sessionLine, h("span", { class: "grow" }), startBtn), flow, usageBox);
    return box;
  }
  const render0 = render;

  // What runs, and whether a newer one exists. Installed and kept
  // current by the installation; the button is for not waiting a day.
  function binaryLine(name, d) {
    const line = h("p", { class: "small binary-line" });
    const btn = h("button", { type: "button", class: "btn sm" }, icon("download"), d && d.installed ? t("modules.binaryUpdate") : t("modules.binaryInstall"));
    btn.addEventListener("click", async () => {
      btn.classList.add("busy");
      try { await api.post(`/deps/${enc(name)}/install`); toast(t("modules.binaryDone", { name }), "ok"); load(body, render0); }
      catch (e) { toastError(e); } finally { btn.classList.remove("busy"); }
    });
    if (!d || !d.installed) line.append(h("span", { class: "muted" }, t("modules.binaryMissing", { name })), " ", btn);
    else {
      line.append(h("code", null, name + " " + (d.version || "?")));
      if (d.outdated) line.append(" ", h("span", { class: "badge warn" }, t("modules.binaryOutdated", { v: d.latest })), " ", btn);
      else if (d.managed) line.append(" ", h("span", { class: "muted" }, t("modules.binaryManaged")));
      else line.append(" ", h("span", { class: "muted" }, t("modules.binaryElsewhere", { path: d.path })));
    }
    return line;
  }
  // A need the module knows the values of (an agent's models) is a list,
  // not a box to type a model's name from memory into. A value stored
  // before, or one the list no longer has, stays on it as it is.
  function choices(n) {
    const label = (v) => (n.labels && n.labels[v] ? `${v} — ${n.labels[v]}` : v);
    const sel = h("select", { class: "mono" },
      h("option", { value: "" }, t("modules.firstListed")),
      ...n.options.map((v) => h("option", { value: v }, label(v))));
    if (n.value && !n.options.includes(n.value)) sel.append(h("option", { value: n.value }, n.value));
    sel.value = n.value || "";
    return sel;
  }
  function stateBadge(m) {
    return m.enabled ? h("span", { class: "mod-on" }, h("span", { class: "dot ok" }), t("modules.on")) : h("span", { class: "mod-off" }, t("modules.off"));
  }

  load(body, render);
  // A module event from elsewhere (the CLI, another tab) refreshes the
  // cards, unless the operator is typing or reading result lines here:
  // rebuilding would wipe the very lines they asked for.
  subs.push(onEvent("module.", () => {
    const typing = document.activeElement && document.activeElement.tagName === "INPUT";
    const reading = body.querySelector(".checklist li");
    if (!typing && !reading) load(body, render);
  }));
  return { el: p.el, destroy() { subs.forEach((u) => u()); } };
}
