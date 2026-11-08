package backups

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The browser for real, on SQLite where sqlite3 is installed: a declared
// database and one only found in the folder are listed, their tables
// read, a query answered in column order, and a write refused by the
// database itself.
func TestTheBrowserReadsADatabaseAndRefusesToWriteIt(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 is not installed")
	}
	f := newFixture(t, `{"version": 2, "backups": {"app": {"engine": "sqlite", "file": "data/app.db"}, "m": {"engine": "mongodb"}}}`)
	mk := func(rel, sql string) {
		p := filepath.Join(f.dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if out, err := exec.Command("sqlite3", p, sql).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	mk("data/app.db", "CREATE TABLE orders(id INTEGER, customer TEXT, note TEXT); INSERT INTO orders VALUES (1,'Ana','a, \"quoted\"'),(2,'Luis',NULL); CREATE VIEW names AS SELECT customer FROM orders;")
	mk("cache/sessions.sqlite", "CREATE TABLE s(k TEXT);")
	os.MkdirAll(filepath.Join(f.dir, "node_modules", "x"), 0o755)
	mk("node_modules/x/test.db", "CREATE TABLE z(a);")
	os.WriteFile(filepath.Join(f.dir, "notes.db"), []byte("not a database"), 0o644)

	dbs, err := f.k.Databases("shop.example")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range dbs {
		names = append(names, d.Name)
	}
	if got := strings.Join(names, ","); got != "app,file:cache/sessions.sqlite" {
		t.Fatalf("databases %q", got)
	}
	ctx := context.Background()
	tables, err := f.k.Tables(ctx, "shop.example", "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(tables.Rows) != 2 || tables.Rows[0][0] != "names" || tables.Rows[1][0] != "orders" {
		t.Fatalf("tables %+v", tables)
	}
	res, err := f.k.Query(ctx, "shop.example", "app", "SELECT id, customer, note FROM orders ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Columns, ",") != "id,customer,note" || len(res.Rows) != 2 || res.Rows[0][2] != `a, "quoted"` || res.Rows[1][1] != "Luis" {
		t.Fatalf("query %+v", res)
	}
	_, err = f.k.Query(ctx, "shop.example", "app", "DELETE FROM orders")
	var qe *QueryError
	if !errors.As(err, &qe) || !strings.Contains(strings.ToLower(qe.Message), "readonly") {
		t.Fatalf("a write was not refused by the database: %v", err)
	}
	if out, _ := exec.Command("sqlite3", filepath.Join(f.dir, "data/app.db"), "SELECT count(*) FROM orders").Output(); strings.TrimSpace(string(out)) != "2" {
		t.Fatalf("the rows are gone: %s", out)
	}
	if _, err := f.k.Query(ctx, "shop.example", "file:cache/sessions.sqlite", "SELECT * FROM s"); err != nil {
		t.Fatalf("a database found in the folder: %v", err)
	}
	for _, name := range []string{"file:../../etc/x.db", "file:notes.db", "file:node_modules/x/test.db", "nope"} {
		if _, err := f.k.Query(ctx, "shop.example", name, "SELECT 1"); !errors.Is(err, ErrNoDatabase) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := f.k.Query(ctx, "shop.example", "m", "SELECT 1"); err == nil {
		t.Error("a mongodb backup was queried with SQL")
	}
}

func TestAQueryIsCutToItsFirstRows(t *testing.T) {
	var b strings.Builder
	b.WriteString("n\n")
	for i := 0; i < MaxQueryRows+5; i++ {
		b.WriteString("1\n")
	}
	cols, rows, cut := parseOutput("sqlite", []byte(b.String()))
	if len(cols) != 1 || len(rows) != MaxQueryRows || !cut {
		t.Fatalf("%d rows, cut %v", len(rows), cut)
	}
}

func TestMySQLBatchOutputIsUnescaped(t *testing.T) {
	cols, rows, _ := parseOutput("mysql", []byte("id\tnote\n1\ta\\tb\\nc\\\\d\n2\tNULL\n"))
	if strings.Join(cols, ",") != "id,note" || rows[0][1] != "a\tb\nc\\d" || rows[1][1] != "NULL" {
		t.Fatalf("%q %q", cols, rows)
	}
}
