// A site's databases, read where they are: pick one (the ones its
// backups name and the SQLite files found in its folder), see its tables,
// click one for its first rows, or write a query. The database itself
// refuses a write (internal/backups, query.go); this is for looking.

import { icon } from "../lib/icons.js";
import { h, clear, append } from "../lib/dom.js";
import { t } from "../lib/i18n.js";
import { api, enc } from "../lib/api.js";
import { empty, errorBox, notice, table, cell } from "../components/page.js";
import { field } from "../components/dialog.js";

// An identifier quoted for the engine: a table called "order" (a word
// of SQL) or with a dot in its name still reads.
function quote(engine, name) {
  if (engine === "mysql") return "`" + name.replace(/`/g, "``") + "`";
  if (engine === "postgres" && name.includes(".")) {
    return name.split(".").map((p) => '"' + p.replace(/"/g, '""') + '"').join(".");
  }
  return '"' + name.replace(/"/g, '""') + '"';
}

export function databaseTab(domain) {
  const base = `/sites/${enc(domain)}/databases`;
  const el = h("div", { class: "cards" });
  const pick = h("select");
  const tablesEl = h("div", { class: "db-tables" });
  const sql = h("textarea", { class: "db-sql mono", rows: "4", spellcheck: "false", placeholder: "SELECT * FROM …" });
  const run = h("button", { type: "button", class: "btn primary" }, icon("play"), t("db.run"));
  const out = h("div", { class: "db-out" });
  let dbs = [];

  const current = () => dbs.find((d) => d.name === pick.value);

  async function query(text) {
    if (!text.trim() || run.classList.contains("busy")) return;
    run.classList.add("busy");
    clear(out);
    try {
      const r = await api.post(base + "/query", { database: pick.value, sql: text });
      drawResult(r);
    } catch (e) {
      append(out, [notice("bad", e.message || String(e))]);
    } finally {
      run.classList.remove("busy");
    }
  }

  function drawResult(r) {
    const cols = r.columns || [];
    const rows = r.rows || [];
    const meta = h("p", { class: "small muted" }, t("db.rows", { n: rows.length, ms: r.ms }), r.truncated ? " · " + t("db.truncated") : "");
    if (!cols.length) {
      append(clear(out), [meta, h("p", { class: "small muted" }, t("db.noRows"))]);
      return;
    }
    const tb = table(cols.map((c) => ({ label: c })), rows.map((row) => h("tr", null, ...cols.map((c, i) => cell(row[i] === undefined ? "" : row[i], c, "mono")))));
    append(clear(out), [meta, tb.el]);
  }

  async function loadTables() {
    clear(tablesEl);
    clear(out);
    const d = current();
    if (!d) return;
    try {
      const r = await api.get(base + "/tables?database=" + encodeURIComponent(d.name));
      const rows = r.rows || [];
      if (!rows.length) {
        tablesEl.append(h("p", { class: "small muted" }, t("db.noTables")));
        return;
      }
      tablesEl.append(h("ul", { class: "db-table-list" }, ...rows.map(([name, type]) => {
        const b = h("button", { type: "button", class: "btn sm ghost" }, icon(type && type.includes("view") ? "eye" : "list", { size: 14 }), name);
        b.addEventListener("click", () => {
          sql.value = `SELECT * FROM ${quote(d.engine, name)} LIMIT 100`;
          query(sql.value);
        });
        return h("li", null, b);
      })));
    } catch (e) {
      tablesEl.append(notice("bad", e.message || String(e)));
    }
  }

  run.addEventListener("click", () => query(sql.value));
  sql.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) {
      e.preventDefault();
      query(sql.value);
    }
  });
  pick.addEventListener("change", loadTables);

  (async () => {
    try {
      dbs = await api.get(base);
    } catch (e) {
      el.append(errorBox(e));
      return;
    }
    if (!dbs.length) {
      el.append(h("div", { class: "card" }, empty(t("db.emptyTitle"), t("db.emptyHint"), null, "box")));
      return;
    }
    for (const d of dbs) {
      const label = d.name.startsWith("file:") ? d.file : `${d.name} (${d.engine}${d.service ? " · " + d.service : ""})`;
      pick.append(h("option", { value: d.name }, label));
    }
    append(el, [
      h("section", { class: "card site-section" },
        h("h2", null, t("site.tab.database")),
        h("p", { class: "small muted" }, t("db.hint")),
        field(t("db.database"), pick),
        tablesEl),
      h("section", { class: "card site-section" },
        field(t("db.query"), sql, t("db.queryHint")),
        h("div", { class: "row-actions" }, run),
        out),
    ]);
    loadTables();
  })();
  return { el };
}
