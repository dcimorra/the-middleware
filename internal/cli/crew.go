package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/dcimorra/the-middleware/internal/crew"
)

const crewUsage = `usage: middleware crew <command> [--agent <id>]

  list                the operator's agents and where each stands
  status              an agent: goal, where it stands, requests, vault names
  journal [--limit N] what it did, wake by wake
  on | off | stop | wake
                      switch it on (it looks at once), off, stop it now, or have it look
  request --title T [--field KEY] [--secret KEY] [--option O] [--urgent] -
                      (the agent) ask the operator for something, the text on stdin
  requests [<id>]     its requests and what the operator answered
  answer <id> [--text T] [--choice O] [--set KEY=value] [--dismiss]
                      answer one of its requests
  vault               the names in its vault
  vault set KEY -     keep a value in its vault, read from stdin
  vault rm KEY        remove a value from its vault

Inside an agent's turn --agent is its own (MIDDLEWARE_CREW).
`

// list is a flag that may be given more than once.
type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

// crew is `middleware crew`: the operator's commands and the agent's own
// (chat.CrewCommands), the same API for both; the house decides which
// caller may do what.
func (e *Env) crew(args []string) error {
	if len(args) == 0 || args[0] == "help" {
		fmt.Fprint(e.Stderr, crewUsage)
		return nil
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("crew "+sub, flag.ContinueOnError)
	var c common
	c.bind(fs)
	agentID := fs.String("agent", os.Getenv("MIDDLEWARE_CREW"), "the agent")
	limit := fs.Int("limit", 0, "how many")
	title := fs.String("title", "", "request: its title")
	urgent := fs.Bool("urgent", false, "request: a check only a person can pass")
	var fields, secretFields, options, sets list
	fs.Var(&fields, "field", "request: a vault name it wants")
	fs.Var(&secretFields, "secret", "request: a vault name it wants, hidden as typed")
	fs.Var(&options, "option", "request: a choice")
	fs.Var(&sets, "set", "answer: KEY=value into the vault")
	text := fs.String("text", "", "answer: the words")
	choice := fs.String("choice", "", "answer: the option picked")
	dismiss := fs.Bool("dismiss", false, "answer: dismiss it")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	cl, err := e.client(c)
	if err != nil {
		return err
	}
	if sub == "list" {
		var raw []byte
		if err := cl.Do("GET", "/api/v1/crew", nil, &raw); err != nil {
			return err
		}
		if c.json {
			e.printJSON(raw)
			return nil
		}
		var all []crew.Status
		if err := json.Unmarshal(raw, &all); err != nil {
			return err
		}
		if len(all) == 0 {
			fmt.Fprintln(e.Stdout, "no agents yet: create one in the panel, Agents")
			return nil
		}
		rows := [][]string{}
		for _, st := range all {
			rows = append(rows, []string{st.Agent.ID, st.Agent.Name, crewState(st), strconv.Itoa(len(st.Requests))})
		}
		Table(e.Stdout, []string{"ID", "NAME", "STATE", "OPEN REQUESTS"}, rows)
		return nil
	}
	if *agentID == "" {
		return fmt.Errorf("which agent? --agent <id> (middleware crew list names them)")
	}
	base := "/api/v1/crew/" + url.PathEscape(*agentID)
	stdin := func() (string, error) {
		b, err := io.ReadAll(io.LimitReader(e.Stdin, 1<<20))
		return string(b), err
	}
	switch sub {
	case "status":
		var raw []byte
		if err := cl.Do("GET", base, nil, &raw); err != nil {
			return err
		}
		if c.json {
			e.printJSON(raw)
			return nil
		}
		var st crew.Status
		if err := json.Unmarshal(raw, &st); err != nil {
			return err
		}
		fmt.Fprintf(e.Stdout, "%s (%s): %s\n", st.Agent.Name, st.Agent.ID, crewState(st))
		if st.State.LastResult != "" {
			fmt.Fprintf(e.Stdout, "last turn: %s\n", st.State.LastResult)
		}
		if st.ContextPercent >= 0 {
			fmt.Fprintf(e.Stdout, "context: %d%% of the window\n", st.ContextPercent)
		}
		for _, w := range st.Quota {
			fmt.Fprintf(e.Stdout, "subscription %s: %.0f%% used\n", w.Name, w.Used)
		}
		fmt.Fprintf(e.Stdout, "vault: %s\n", orNone(strings.Join(st.Vault, ", ")))
		fmt.Fprintf(e.Stdout, "room: %d of %d MiB in its desk and home\n", st.DiskUsed, st.Agent.Limits().DiskMB)
		fmt.Fprintf(e.Stdout, "\ngoal:\n%s\n", strings.TrimSpace(st.Agent.Goal))
		for _, r := range st.Requests {
			if r.Status == crew.StatusOpen {
				fmt.Fprintf(e.Stdout, "open request %s: %s\n", r.ID, r.Title)
			}
		}
		return nil
	case "journal", "log":
		n := *limit
		if n <= 0 {
			n = 50
		}
		var raw []byte
		if err := cl.Do("GET", base+"/journal?limit="+strconv.Itoa(n), nil, &raw); err != nil {
			return err
		}
		if c.json {
			e.printJSON(raw)
			return nil
		}
		var entries []crew.Entry
		if err := json.Unmarshal(raw, &entries); err != nil {
			return err
		}
		for _, en := range entries {
			line := en.At.Local().Format("01-02 15:04") + "  " + fmt.Sprintf("%-8s", en.Kind) + " " + en.Message
			if en.Next != nil {
				line += " (next " + en.Next.Local().Format("15:04") + ")"
			}
			fmt.Fprintln(e.Stdout, line)
			for _, cmd := range en.Commands {
				fmt.Fprintln(e.Stdout, "              $ "+cmd)
			}
		}
		return nil
	case "on", "off", "stop", "wake":
		done := map[string]string{"on": "switched on: it looks now", "off": "switched off: a turn at work ends on its own", "stop": "stopped: switched off and its turn cut", "wake": "it looks on its next pass"}[sub]
		return e.simple(c, cl, "POST", base+"/"+sub, nil, done)
	case "request", "ask":
		if len(pos) != 1 || pos[0] != "-" || strings.TrimSpace(*title) == "" {
			return fmt.Errorf("usage: middleware crew request --title <title> [--field KEY] [--secret KEY] [--option <choice>] [--urgent] - (the explanation on stdin)")
		}
		body, err := stdin()
		if err != nil {
			return err
		}
		req := crew.Request{Title: *title, Text: body, Options: options, Urgent: *urgent}
		for _, k := range fields {
			req.Fields = append(req.Fields, crew.Field{Key: k})
		}
		for _, k := range secretFields {
			req.Fields = append(req.Fields, crew.Field{Key: k, Secret: true})
		}
		var out crew.Request
		if err := cl.Do("POST", base+"/requests", req, &out); err != nil {
			return err
		}
		how := "the operator sees it in the panel"
		if out.Urgent {
			how = "the operator's phone was told at once"
		}
		fmt.Fprintf(e.Stdout, "request %s filed: %s; you are woken when they answer\n", out.ID, how)
		return nil
	case "requests":
		var raw []byte
		if err := cl.Do("GET", base+"/requests", nil, &raw); err != nil {
			return err
		}
		if c.json {
			e.printJSON(raw)
			return nil
		}
		var all []crew.Request
		if err := json.Unmarshal(raw, &all); err != nil {
			return err
		}
		if len(pos) == 1 {
			for _, r := range all {
				if r.ID != pos[0] {
					continue
				}
				fmt.Fprintf(e.Stdout, "%s · %s · %s\n# %s\n\n%s\n", r.ID, r.Status, r.At.Local().Format("2006-01-02 15:04"), r.Title, r.Text)
				if len(r.Filled) > 0 {
					fmt.Fprintf(e.Stdout, "\ninto the vault: %s\n", strings.Join(r.Filled, ", "))
				}
				if r.Choice != "" {
					fmt.Fprintf(e.Stdout, "chose: %s\n", r.Choice)
				}
				if r.Answer != "" {
					fmt.Fprintf(e.Stdout, "answer: %s\n", r.Answer)
				}
				return nil
			}
			return fmt.Errorf("no request %s", pos[0])
		}
		if len(all) == 0 {
			fmt.Fprintln(e.Stdout, "no requests")
			return nil
		}
		rows := [][]string{}
		for _, r := range all {
			rows = append(rows, []string{r.ID, r.Status, r.At.Local().Format("01-02 15:04"), r.Title})
		}
		Table(e.Stdout, []string{"ID", "STATE", "AT", "TITLE"}, rows)
		return nil
	case "answer":
		if len(pos) != 1 {
			return fmt.Errorf("usage: middleware crew answer <id> [--text T] [--choice O] [--set KEY=value] [--dismiss]")
		}
		rep := crew.Reply{Answer: *text, Choice: *choice, Dismiss: *dismiss, Values: map[string]string{}}
		for _, kv := range sets {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return fmt.Errorf("--set %q is not KEY=value", kv)
			}
			rep.Values[strings.ToUpper(k)] = v
		}
		return e.simple(c, cl, "POST", base+"/requests/"+url.PathEscape(pos[0])+"/answer", rep, "answered: it is woken within a minute")
	case "vault":
		if len(pos) > 0 {
			switch {
			case pos[0] == "set" && len(pos) == 3 && pos[2] == "-":
				v, err := stdin()
				if err != nil {
					return err
				}
				// A value piped in ends with the newline echo put there.
				v = strings.TrimRight(v, "\r\n")
				return e.simple(c, cl, "PUT", base+"/vault/"+url.PathEscape(strings.ToUpper(pos[1])), map[string]string{"value": v}, strings.ToUpper(pos[1])+" kept in the vault")
			case pos[0] == "rm" && len(pos) == 2:
				return e.simple(c, cl, "DELETE", base+"/vault/"+url.PathEscape(strings.ToUpper(pos[1])), nil, strings.ToUpper(pos[1])+" removed from the vault")
			}
			return fmt.Errorf("usage: middleware crew vault [set KEY - | rm KEY]")
		}
		var keys []string
		if err := cl.Do("GET", base+"/vault", nil, &keys); err != nil {
			return err
		}
		if len(keys) == 0 {
			fmt.Fprintln(e.Stdout, "the vault is empty")
			return nil
		}
		for _, k := range keys {
			fmt.Fprintln(e.Stdout, k)
		}
		return nil
	}
	return fmt.Errorf("unknown crew command %q\n%s", sub, crewUsage)
}

func crewState(st crew.Status) string {
	switch {
	case !st.Agent.Enabled && st.Running:
		return "off, a turn finishing"
	case !st.Agent.Enabled:
		return "off"
	case st.Running:
		return "working"
	case st.State.Skipped != "":
		return "held back: " + st.State.Skipped
	case !st.State.NextWake.IsZero():
		return "on, next wake " + st.State.NextWake.Local().Format("01-02 15:04")
	}
	return "on"
}

func orNone(s string) string {
	if s == "" {
		return "empty"
	}
	return s
}
