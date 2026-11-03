package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"time"

	"github.com/dcimorra/the-middleware/internal/backups"
	"github.com/dcimorra/the-middleware/internal/manifest"
)

const siteBackupsUsage = `usage: middleware site backups <domain> [command]

  list                              each backup (from the manifest's backups) with its copies
  run <name>                        take a copy now and wait for it
  restore <name> <copy> [--no-safety-copy]
                                    put a copy back; a copy of the database as it is now
                                    is taken first, and nothing is restored if that fails
  download <name> <copy> [file]     save a copy (gzip) to file, by default its own name
  rm <name> <copy>                  delete a copy
  log <name> [--lines 200]          what its dumps and restores said

What is copied, when and how many are kept is the manifest's "backups"
(docs/manifest.md): write it in site.json, then run it once.
`

// siteBackups is `middleware site backups`: a site's database copies
// through /sites/{domain}/backups, which a site's agent turn reaches on
// the socket. run and restore wait for the end, so the agent that ran
// one knows how it went in the same command.
func (e *Env) siteBackups(c common, cl *Client, domain string, args []string, lines int, noSafety bool) error {
	verb := "list"
	if len(args) > 0 {
		verb, args = args[0], args[1:]
	}
	base := "/api/v1/sites/" + url.PathEscape(domain) + "/backups"
	need := func(n int, what string) error {
		if len(args) < n || len(args) > n+boolInt(verb == "download") {
			fmt.Fprint(e.Stderr, siteBackupsUsage)
			return fmt.Errorf("site backups %s: %s", verb, what)
		}
		if !manifest.ValidBackupName(args[0]) {
			return fmt.Errorf("site backups %s: %q is not a backup's name", verb, args[0])
		}
		if n > 1 && !backups.ValidCopy(args[1]) {
			return fmt.Errorf("site backups %s: %q is not a copy (see `middleware site backups %s`)", verb, args[1], domain)
		}
		return nil
	}
	switch verb {
	case "list", "ls":
		var raw []byte
		if err := cl.Do("GET", base, nil, &raw); err != nil {
			return err
		}
		if c.json {
			e.printJSON(raw)
			return nil
		}
		var list []backups.Status
		if err := json.Unmarshal(raw, &list); err != nil {
			return err
		}
		if len(list) == 0 {
			fmt.Fprintf(e.Stdout, "no backups for %s: write one in site.json (\"backups\"), see `middleware site backups %s help`\n", domain, domain)
			return nil
		}
		for i, st := range list {
			if i > 0 {
				fmt.Fprintln(e.Stdout)
			}
			e.printBackup(st)
		}
		return nil
	case "run":
		if err := need(1, "a backup's name is required"); err != nil {
			return err
		}
		var cp backups.Copy
		if err := cl.Do("POST", base+"/"+args[0]+"/run", map[string]any{"wait": true}, &cp); err != nil {
			return err
		}
		if c.json {
			data, _ := json.Marshal(cp)
			e.printJSON(data)
			return nil
		}
		fmt.Fprintf(e.Stdout, "Copied: %s (%s compressed)\n", cp.ID, bytesWords(cp.Bytes))
		return nil
	case "restore":
		if err := need(2, "a backup's name and a copy are required"); err != nil {
			return err
		}
		var res backups.Result
		if err := cl.Do("POST", base+"/"+args[0]+"/copies/"+args[1]+"/restore", map[string]any{"wait": true, "no_safety_copy": noSafety}, &res); err != nil {
			return err
		}
		if c.json {
			data, _ := json.Marshal(res)
			e.printJSON(data)
			return nil
		}
		fmt.Fprintf(e.Stdout, "Restored %s in %s", args[1], time.Duration(res.Seconds*float64(time.Second)).Round(time.Second))
		if !noSafety {
			fmt.Fprint(e.Stdout, "; what was there before is the newest pre-restore copy")
		}
		fmt.Fprintln(e.Stdout)
		return nil
	case "download", "get":
		if err := need(2, "a backup's name and a copy are required"); err != nil {
			return err
		}
		file := args[1]
		if len(args) == 3 {
			file = args[2]
		}
		body, err := cl.Stream(context.Background(), base+"/"+args[0]+"/copies/"+args[1])
		if err != nil {
			return err
		}
		defer body.Close()
		// 0600: a database's whole content.
		f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		n, err := io.Copy(f, body)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(file)
			return err
		}
		fmt.Fprintf(e.Stdout, "Saved %s (%s, gzip): never commit it, it is the database's data\n", file, bytesWords(n))
		return nil
	case "rm", "remove", "delete":
		if err := need(2, "a backup's name and a copy are required"); err != nil {
			return err
		}
		return e.simple(c, cl, "DELETE", base+"/"+args[0]+"/copies/"+args[1], nil, "Deleted "+args[1])
	case "log", "logs":
		if err := need(1, "a backup's name is required"); err != nil {
			return err
		}
		q := ""
		if lines > 0 {
			q = fmt.Sprintf("?lines=%d", lines)
		}
		var raw []byte
		if err := cl.Do("GET", base+"/"+args[0]+"/log"+q, nil, &raw); err != nil {
			return err
		}
		_, err := e.Stdout.Write(raw)
		return err
	case "help":
		fmt.Fprint(e.Stdout, siteBackupsUsage)
		return nil
	}
	fmt.Fprint(e.Stderr, siteBackupsUsage)
	return fmt.Errorf("site backups: unknown command %q", verb)
}

func (e *Env) printBackup(st backups.Status) {
	head := st.Name
	if b := st.Config; b != nil {
		where := b.Service
		if b.Engine == "sqlite" {
			where = b.File
		}
		if where != "" {
			head += " · " + b.Engine + " in " + where
		} else {
			head += " · " + b.Engine
		}
		if b.Database != "" {
			head += " · database " + b.Database
		}
		switch {
		case b.Disabled:
			head += " · disabled"
		case b.Schedule == "":
			head += " · by hand only"
		default:
			head += " · " + b.Schedule
			if b.Timezone != "" {
				head += " " + b.Timezone
			}
			if st.Next != nil {
				head += " (next " + st.Next.Local().Format("2006-01-02 15:04") + ")"
			}
		}
		head += fmt.Sprintf(" · keeps %d", b.KeepCopies())
	}
	fmt.Fprintln(e.Stdout, head)
	if st.Error != "" {
		fmt.Fprintln(e.Stdout, "  "+st.Error)
	}
	if st.Running != "" {
		fmt.Fprintln(e.Stdout, "  running: "+st.Running)
	}
	if l := st.Last; l != nil {
		if l.OK {
			fmt.Fprintf(e.Stdout, "  last copy: %s ok (%s)\n", l.At.Local().Format("2006-01-02 15:04"), bytesWords(l.Bytes))
		} else {
			fmt.Fprintf(e.Stdout, "  last copy: %s FAILED: %s\n", l.At.Local().Format("2006-01-02 15:04"), oneLine(l.Error))
		}
	}
	if l := st.LastRestore; l != nil {
		state := "ok"
		if !l.OK {
			state = "FAILED: " + oneLine(l.Error)
		}
		fmt.Fprintf(e.Stdout, "  last restore: %s of %s %s\n", l.At.Local().Format("2006-01-02 15:04"), l.Copy, state)
	}
	if len(st.Copies) == 0 {
		fmt.Fprintln(e.Stdout, "  no copies yet")
		return
	}
	rows := make([][]string, 0, len(st.Copies))
	for _, cp := range st.Copies {
		rows = append(rows, []string{"  " + cp.ID, cp.At.Local().Format("2006-01-02 15:04:05"), cp.Kind, bytesWords(cp.Bytes)})
	}
	Table(e.Stdout, []string{"  COPY", "TAKEN", "KIND", "SIZE"}, rows)
}

func bytesWords(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
