// Result lines from verify, enable and move: each one is a state (ok,
// skip, fail) with an icon and the word, so the operator reads what was
// probed and what could not be, line by line as they arrive.

import { h, clear } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { icon } from "../lib/icons.js";

const WORD = { ok: "check.ok", skip: "check.skip", fail: "check.fail", pending: "check.pending" };
const MARK = { ok: "check", skip: "minus", fail: "x", pending: "" };

function stateOf(line) {
  if (typeof line.ok === "boolean") return line.ok ? "ok" : "fail";
  const s = line.state || line.status || line.kind || line.result || (line.skip ? "skip" : "");
  if (s === "ok" || s === "skip" || s === "fail") return s;
  if (s === "skipped") return "skip";
  if (s === "error" || s === "failed") return "fail";
  return "pending";
}

export function checklist(lines) {
  const ul = h("ul", { class: "checklist" });
  const c = {
    el: ul,
    set(items) {
      clear(ul);
      for (const line of items || []) c.add(line);
      return c;
    },
    add(line) {
      const s = stateOf(line);
      // "skip" is the page's skip link, positioned off screen: a skipped
      // line carrying that class was never seen.
      ul.append(h("li", { class: s === "skip" ? "skipped" : s },
        h("span", { class: "mark", "aria-hidden": "true" }, MARK[s] ? icon(MARK[s]) : null),
        h("span", { class: "word" }, t(WORD[s])),
        h("span", { class: "text" }, line.text || line.message || ""),
      ));
      return c;
    },
  };
  if (lines) c.set(lines);
  return c;
}

export function summary(lines) {
  const n = { ok: 0, skip: 0, fail: 0 };
  for (const l of lines || []) n[stateOf(l)] = (n[stateOf(l)] || 0) + 1;
  return n;
}
