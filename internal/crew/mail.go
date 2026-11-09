package crew

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/autopilot"
)

// An agent's email, written against the standard library: SMTP to send
// (net/smtp) and the few IMAP commands that read an inbox. The house
// does it on the agent's behalf with the vault's credentials, so the
// agent asks for a message by its uid and gets text back, and a reply
// that is a verification code is one command away.

// MailConfig is what the vault says of the agent's account.
type MailConfig struct {
	SMTPHost, SMTPUser, SMTPPassword, From string
	SMTPPort                               int
	IMAPHost, IMAPUser, IMAPPassword       string
	IMAPPort                               int
	// TLS, when set, is the TLS configuration the dials use (tests).
	TLS *tls.Config
	// Plain dials IMAP without TLS (tests).
	Plain bool
}

func mailConfig(v map[string]string) MailConfig {
	c := MailConfig{SMTPHost: v["SMTP_HOST"], SMTPUser: v["SMTP_USER"], SMTPPassword: v["SMTP_PASSWORD"], From: v["SMTP_FROM"],
		IMAPHost: v["IMAP_HOST"], IMAPUser: v["IMAP_USER"], IMAPPassword: v["IMAP_PASSWORD"]}
	c.SMTPPort, _ = strconv.Atoi(v["SMTP_PORT"])
	c.IMAPPort, _ = strconv.Atoi(v["IMAP_PORT"])
	if c.SMTPPort == 0 {
		c.SMTPPort = 465
	}
	if c.IMAPPort == 0 {
		c.IMAPPort = 993
	}
	if c.From == "" {
		c.From = c.SMTPUser
	}
	if c.IMAPHost == "" {
		c.IMAPHost = c.SMTPHost
	}
	if c.IMAPUser == "" {
		c.IMAPUser = c.SMTPUser
	}
	if c.IMAPPassword == "" {
		c.IMAPPassword = c.SMTPPassword
	}
	return c
}

// Outgoing is a message to send.
type Outgoing struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
	HTML    bool   `json:"html,omitempty"`
}

// maxMailPerDay bounds what an agent sends: enough for a person's
// correspondence, far from what gets an address taken for a spammer's.
const maxMailPerDay = 50

// SendMail sends a message from the agent's address.
func (c *Crew) SendMail(ctx context.Context, id string, m Outgoing) error {
	c.mu.Lock()
	mb := c.get(id)
	if mb == nil {
		c.mu.Unlock()
		return ErrNoAgent
	}
	now := c.now()
	kept := mb.st.Mail[:0]
	for _, t := range mb.st.Mail {
		if now.Sub(t) < 24*time.Hour {
			kept = append(kept, t)
		}
	}
	mb.st.Mail = kept
	if len(kept) >= maxMailPerDay {
		c.mu.Unlock()
		return fmt.Errorf("you sent %d messages in the last 24 hours, the most an agent may", maxMailPerDay)
	}
	c.mu.Unlock()
	cfg := mailConfig(c.Env(id))
	if c.mailTLS != nil {
		cfg.TLS = c.mailTLS
	}
	if err := sendMail(ctx, cfg, m); err != nil {
		return err
	}
	c.mu.Lock()
	if mb := c.get(id); mb != nil {
		mb.st.Mail = append(mb.st.Mail, now)
		c.saveStateLocked(mb)
		c.note(mb, Entry{Kind: KindMail, Message: autopilot.Clip("to "+m.To+": "+m.Subject, 300)})
	}
	c.mu.Unlock()
	return nil
}

func sendMail(ctx context.Context, cfg MailConfig, m Outgoing) error {
	if cfg.SMTPHost == "" || cfg.From == "" {
		return errors.New("the vault lacks SMTP_HOST, SMTP_USER or SMTP_FROM: ask the operator for your account's details")
	}
	to, err := mail.ParseAddress(m.To)
	if err != nil {
		return fmt.Errorf("%q is not an address: %v", m.To, err)
	}
	from, err := mail.ParseAddress(cfg.From)
	if err != nil {
		return fmt.Errorf("SMTP_FROM %q is not an address: %v", cfg.From, err)
	}
	if strings.ContainsAny(m.Subject, "\r\n") {
		return errors.New("the subject is one line")
	}
	if len(m.Body) > 1<<20 {
		return errors.New("the body is longer than 1 MB")
	}
	msg := compose(from, to, m)
	addr := net.JoinHostPort(cfg.SMTPHost, strconv.Itoa(cfg.SMTPPort))
	tlsCfg := cfg.TLS
	if tlsCfg == nil {
		tlsCfg = &tls.Config{ServerName: cfg.SMTPHost, MinVersion: tls.VersionTLS12}
	}
	d := net.Dialer{Timeout: 20 * time.Second}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var conn net.Conn
	if cfg.SMTPPort == 465 {
		conn, err = (&tls.Dialer{NetDialer: &d, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	cl, err := smtp.NewClient(conn, cfg.SMTPHost)
	if err != nil {
		conn.Close()
		return fmt.Errorf("smtp: %w", err)
	}
	defer cl.Close()
	if cfg.SMTPPort != 465 {
		if ok, _ := cl.Extension("STARTTLS"); ok {
			if err := cl.StartTLS(tlsCfg); err != nil {
				return fmt.Errorf("smtp starttls: %w", err)
			}
		}
	}
	if cfg.SMTPUser != "" {
		// net/smtp refuses PLAIN over a connection without TLS, but to
		// this machine: a password does not cross the network in clear.
		if err := cl.Auth(smtp.PlainAuth("", cfg.SMTPUser, cfg.SMTPPassword, cfg.SMTPHost)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := cl.Mail(from.Address); err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	if err := cl.Rcpt(to.Address); err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	w, err := cl.Data()
	if err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	return cl.Quit()
}

func compose(from, to *mail.Address, m Outgoing) []byte {
	var b bytes.Buffer
	var id [12]byte
	_, _ = rand.Read(id[:])
	domain := "localhost"
	if _, d, ok := strings.Cut(from.Address, "@"); ok {
		domain = d
	}
	ctype := "text/plain"
	if m.HTML {
		ctype = "text/html"
	}
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@%s>\r\nMIME-Version: 1.0\r\nContent-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n",
		from.String(), to.String(), mime.QEncoding.Encode("utf-8", m.Subject), time.Now().Format(time.RFC1123Z), hex.EncodeToString(id[:]), domain, ctype)
	qp := quotedprintable.NewWriter(&b)
	_, _ = qp.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(m.Body, "\r\n", "\n"), "\n", "\r\n")))
	_ = qp.Close()
	b.WriteString("\r\n")
	return b.Bytes()
}

// Message is a message of the inbox as listed.
type Message struct {
	UID     uint32    `json:"uid"`
	From    string    `json:"from"`
	Subject string    `json:"subject"`
	Date    time.Time `json:"date,omitzero"`
	Seen    bool      `json:"seen"`
	// Text is the message as text, when one is read.
	Text string `json:"text,omitempty"`
	// Links are the addresses its HTML part points to: a verification
	// link is often only there.
	Links []string `json:"links,omitempty"`
}

// ListMail is the last limit messages of the inbox, newest first.
func (c *Crew) ListMail(ctx context.Context, id string, unseen bool, limit int) ([]Message, error) {
	if !c.Has(id) {
		return nil, ErrNoAgent
	}
	cfg := mailConfig(c.Env(id))
	if c.mailTLS != nil {
		cfg.TLS = c.mailTLS
	}
	cfg.Plain = c.mailPlain
	return listMail(ctx, cfg, unseen, limit)
}

// ReadMail is one message of the inbox, as text; it is marked as read.
func (c *Crew) ReadMail(ctx context.Context, id string, uid uint32) (Message, error) {
	if !c.Has(id) {
		return Message{}, ErrNoAgent
	}
	cfg := mailConfig(c.Env(id))
	if c.mailTLS != nil {
		cfg.TLS = c.mailTLS
	}
	cfg.Plain = c.mailPlain
	return readMail(ctx, cfg, uid)
}

// imap is a connection to an IMAP server, one command at a time.
type imap struct {
	conn net.Conn
	r    *bufio.Reader
	n    int
}

// imapReply is one untagged response, with its literals ({n} and the n
// bytes after it) taken out of the line.
type imapReply struct {
	line     string
	literals [][]byte
}

func dialIMAP(ctx context.Context, cfg MailConfig) (*imap, error) {
	if cfg.IMAPHost == "" || cfg.IMAPUser == "" {
		return nil, errors.New("the vault lacks IMAP_HOST (or SMTP_HOST) and IMAP_USER (or SMTP_USER): ask the operator for your account's details")
	}
	addr := net.JoinHostPort(cfg.IMAPHost, strconv.Itoa(cfg.IMAPPort))
	d := net.Dialer{Timeout: 20 * time.Second}
	var conn net.Conn
	var err error
	if cfg.Plain {
		conn, err = d.DialContext(ctx, "tcp", addr)
	} else {
		tlsCfg := cfg.TLS
		if tlsCfg == nil {
			tlsCfg = &tls.Config{ServerName: cfg.IMAPHost, MinVersion: tls.VersionTLS12}
		}
		conn, err = (&tls.Dialer{NetDialer: &d, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("imap: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	x := &imap{conn: conn, r: bufio.NewReaderSize(conn, 64<<10)}
	greeting, err := x.r.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("imap: %w", err)
	}
	if !strings.HasPrefix(greeting, "* OK") && !strings.HasPrefix(greeting, "* PREAUTH") {
		conn.Close()
		return nil, fmt.Errorf("imap: the server greeted %q", strings.TrimSpace(greeting))
	}
	if _, err := x.cmd("LOGIN " + quote(cfg.IMAPUser) + " " + quote(cfg.IMAPPassword)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("imap login: %w", err)
	}
	return x, nil
}

// quote is an IMAP quoted string.
func quote(s string) string {
	s = strings.NewReplacer("\r", "", "\n", "", `\`, `\\`, `"`, `\"`).Replace(s)
	return `"` + s + `"`
}

const maxLiteral = 16 << 20

// cmd sends one command and reads to its tagged answer.
func (x *imap) cmd(line string) ([]imapReply, error) {
	x.n++
	tag := fmt.Sprintf("a%03d", x.n)
	if _, err := io.WriteString(x.conn, tag+" "+line+"\r\n"); err != nil {
		return nil, err
	}
	var out []imapReply
	for {
		l, err := x.r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		l = strings.TrimRight(l, "\r\n")
		if rest, ok := strings.CutPrefix(l, tag+" "); ok {
			if strings.HasPrefix(rest, "OK") {
				return out, nil
			}
			return nil, errors.New(rest)
		}
		rep := imapReply{}
		for {
			n, ok := literalAt(l)
			if !ok {
				rep.line += l
				break
			}
			if n > maxLiteral {
				return nil, fmt.Errorf("a literal of %d bytes", n)
			}
			buf := make([]byte, n)
			if _, err := io.ReadFull(x.r, buf); err != nil {
				return nil, err
			}
			rep.line += l
			rep.literals = append(rep.literals, buf)
			if l, err = x.r.ReadString('\n'); err != nil {
				return nil, err
			}
			l = strings.TrimRight(l, "\r\n")
		}
		out = append(out, rep)
	}
}

var literalRe = regexp.MustCompile(`\{(\d+)\}$`)

func literalAt(l string) (int, bool) {
	m := literalRe.FindStringSubmatch(l)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

func (x *imap) close() {
	_, _ = x.cmd("LOGOUT")
	x.conn.Close()
}

var (
	uidRe   = regexp.MustCompile(`UID (\d+)`)
	flagsRe = regexp.MustCompile(`FLAGS \(([^)]*)\)`)
)

func listMail(ctx context.Context, cfg MailConfig, unseen bool, limit int) ([]Message, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	x, err := dialIMAP(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer x.close()
	if _, err := x.cmd("SELECT INBOX"); err != nil {
		return nil, fmt.Errorf("imap select: %w", err)
	}
	crit := "ALL"
	if unseen {
		crit = "UNSEEN"
	}
	reps, err := x.cmd("UID SEARCH " + crit)
	if err != nil {
		return nil, fmt.Errorf("imap search: %w", err)
	}
	var uids []uint32
	for _, r := range reps {
		if rest, ok := strings.CutPrefix(r.line, "* SEARCH"); ok {
			for _, f := range strings.Fields(rest) {
				if n, err := strconv.ParseUint(f, 10, 32); err == nil {
					uids = append(uids, uint32(n))
				}
			}
		}
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] > uids[j] })
	if len(uids) > limit {
		uids = uids[:limit]
	}
	out := []Message{}
	if len(uids) == 0 {
		return out, nil
	}
	set := make([]string, len(uids))
	for i, u := range uids {
		set[i] = strconv.FormatUint(uint64(u), 10)
	}
	reps, err = x.cmd("UID FETCH " + strings.Join(set, ",") + " (UID FLAGS BODY.PEEK[HEADER.FIELDS (FROM SUBJECT DATE)])")
	if err != nil {
		return nil, fmt.Errorf("imap fetch: %w", err)
	}
	for _, r := range reps {
		if !strings.Contains(r.line, "FETCH") {
			continue
		}
		m := Message{}
		if u := uidRe.FindStringSubmatch(r.line); u != nil {
			n, _ := strconv.ParseUint(u[1], 10, 32)
			m.UID = uint32(n)
		}
		if f := flagsRe.FindStringSubmatch(r.line); f != nil {
			m.Seen = strings.Contains(f[1], `\Seen`)
		}
		if len(r.literals) > 0 {
			if msg, err := mail.ReadMessage(bytes.NewReader(append(r.literals[0], '\r', '\n'))); err == nil {
				m.From, m.Subject = decodeHeader(msg.Header.Get("From")), decodeHeader(msg.Header.Get("Subject"))
				m.Date, _ = msg.Header.Date()
			}
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID > out[j].UID })
	return out, nil
}

func readMail(ctx context.Context, cfg MailConfig, uid uint32) (Message, error) {
	x, err := dialIMAP(ctx, cfg)
	if err != nil {
		return Message{}, err
	}
	defer x.close()
	if _, err := x.cmd("SELECT INBOX"); err != nil {
		return Message{}, fmt.Errorf("imap select: %w", err)
	}
	reps, err := x.cmd(fmt.Sprintf("UID FETCH %d (UID FLAGS BODY[])", uid))
	if err != nil {
		return Message{}, fmt.Errorf("imap fetch: %w", err)
	}
	for _, r := range reps {
		if !strings.Contains(r.line, "FETCH") || len(r.literals) == 0 {
			continue
		}
		msg, err := mail.ReadMessage(bytes.NewReader(r.literals[0]))
		if err != nil {
			return Message{}, fmt.Errorf("the message does not parse: %v", err)
		}
		m := Message{UID: uid, From: decodeHeader(msg.Header.Get("From")), Subject: decodeHeader(msg.Header.Get("Subject")), Seen: true}
		m.Date, _ = msg.Header.Date()
		text, htmlBody := bodies(msg.Header.Get("Content-Type"), msg.Header.Get("Content-Transfer-Encoding"), msg.Body, 0)
		if text == "" && htmlBody != "" {
			text = htmlText(htmlBody)
		}
		m.Text = autopilot.Clip(strings.TrimSpace(text), 64<<10)
		if htmlBody != "" {
			m.Links = links(htmlBody)
		}
		return m, nil
	}
	return Message{}, fmt.Errorf("no message with uid %d", uid)
}

var wordDecoder = mime.WordDecoder{}

func decodeHeader(s string) string {
	if d, err := wordDecoder.DecodeHeader(s); err == nil {
		return d
	}
	return s
}

// bodies is a message's text and HTML parts, the first of each, walking
// into multiparts.
func bodies(ctype, cte string, body io.Reader, depth int) (text, htmlBody string) {
	mt, params, err := mime.ParseMediaType(ctype)
	if err != nil || ctype == "" {
		mt = "text/plain"
	}
	if strings.HasPrefix(mt, "multipart/") && depth < 5 {
		mr := multipart.NewReader(body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			t, h := bodies(p.Header.Get("Content-Type"), p.Header.Get("Content-Transfer-Encoding"), p, depth+1)
			if text == "" {
				text = t
			}
			if htmlBody == "" {
				htmlBody = h
			}
		}
		return text, htmlBody
	}
	var r io.Reader = io.LimitReader(body, 4<<20)
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "quoted-printable":
		r = quotedprintable.NewReader(r)
	case "base64":
		r = base64Reader(r)
	}
	b, _ := io.ReadAll(r)
	s := string(b)
	if cs := strings.ToLower(params["charset"]); cs == "iso-8859-1" || cs == "latin1" || cs == "windows-1252" {
		s = latin1(b)
	}
	switch mt {
	case "text/plain":
		return s, ""
	case "text/html":
		return "", s
	}
	return "", ""
}

func latin1(b []byte) string {
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

var (
	scriptRe = regexp.MustCompile(`(?is)<(script|style|head)[^>]*>.*?</(script|style|head)>`)
	breakRe  = regexp.MustCompile(`(?i)<(br|/p|/div|/tr|/h[1-6]|/li)[^>]*>`)
	tagRe    = regexp.MustCompile(`(?s)<[^>]*>`)
	blankRe  = regexp.MustCompile(`\n[ \t]*\n[\s]*`)
	hrefRe   = regexp.MustCompile(`(?i)href\s*=\s*["']([^"']+)["']`)
)

func htmlText(s string) string {
	s = scriptRe.ReplaceAllString(s, "")
	s = breakRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			lines = append(lines, l)
		}
	}
	return blankRe.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
}

func links(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range hrefRe.FindAllStringSubmatch(s, -1) {
		u := html.UnescapeString(m[1])
		if (strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")) && !seen[u] {
			seen[u] = true
			out = append(out, u)
			if len(out) >= 50 {
				break
			}
		}
	}
	return out
}

func base64Reader(r io.Reader) io.Reader { return base64.NewDecoder(base64.StdEncoding, r) }
