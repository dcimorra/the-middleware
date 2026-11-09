package crew

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeSMTP speaks enough SMTP for net/smtp: it advertises AUTH PLAIN and
// keeps the message it is given.
func fakeSMTP(t *testing.T) (port int, got chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got = make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		say := func(s string) { fmt.Fprintf(conn, "%s\r\n", s) }
		say("220 fake ESMTP")
		var data strings.Builder
		for {
			l, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(l))
			switch {
			case strings.HasPrefix(cmd, "EHLO"):
				say("250-fake")
				say("250 AUTH PLAIN")
			case strings.HasPrefix(cmd, "AUTH"):
				say("235 ok")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				say("250 ok")
			case cmd == "DATA":
				say("354 go")
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if l == ".\r\n" {
						break
					}
					data.WriteString(l)
				}
				got <- data.String()
				say("250 kept")
			case cmd == "QUIT":
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, got
}

// fakeIMAP serves one inbox of two messages, the second HTML with a code
// and a link, over as many connections as it is given.
func fakeIMAP(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	headers := map[int]string{
		1: "From: Shop <news@shop.example>\r\nSubject: Weekly news\r\nDate: Mon, 09 Nov 2026 10:00:00 +0000\r\n",
		2: "From: Service <no-reply@service.example>\r\nSubject: =?utf-8?q?Tu_c=C3=B3digo?=\r\nDate: Mon, 09 Nov 2026 11:00:00 +0000\r\n",
	}
	bodies := map[int]string{
		1: headers[1] + "Content-Type: text/plain\r\n\r\nHello.\r\n",
		2: headers[2] + "MIME-Version: 1.0\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n<html><head><style>p{}</style></head><body><p>Your code is <b>482913</b></p><a href=3D\"https://service.example/verify?t=3Dabc&amp;x=3D1\">Verify</a></body></html>\r\n",
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				r := bufio.NewReader(conn)
				fmt.Fprint(conn, "* OK fake IMAP\r\n")
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					f := strings.Fields(l)
					if len(f) < 2 {
						return
					}
					tag, rest := f[0], strings.Join(f[1:], " ")
					switch {
					case strings.HasPrefix(rest, "LOGIN"):
						if !strings.Contains(rest, `"agent@example.com" "p\"w"`) {
							fmt.Fprintf(conn, "%s NO bad login %s\r\n", tag, rest)
							continue
						}
					case strings.HasPrefix(rest, "SELECT"):
						fmt.Fprint(conn, "* 2 EXISTS\r\n")
					case strings.HasPrefix(rest, "UID SEARCH UNSEEN"):
						fmt.Fprint(conn, "* SEARCH 2\r\n")
					case strings.HasPrefix(rest, "UID SEARCH"):
						fmt.Fprint(conn, "* SEARCH 1 2\r\n")
					case strings.HasPrefix(rest, "UID FETCH") && strings.Contains(rest, "HEADER.FIELDS"):
						for _, s := range strings.Split(f[3], ",") {
							n, _ := strconv.Atoi(s)
							flags := `\Seen`
							if n == 2 {
								flags = ""
							}
							fmt.Fprintf(conn, "* %d FETCH (UID %d FLAGS (%s) BODY[HEADER.FIELDS (FROM SUBJECT DATE)] {%d}\r\n%s)\r\n", n, n, flags, len(headers[n]), headers[n])
						}
					case strings.HasPrefix(rest, "UID FETCH"):
						n, _ := strconv.Atoi(f[3])
						fmt.Fprintf(conn, "* %d FETCH (UID %d FLAGS (\\Seen) BODY[] {%d}\r\n%s)\r\n", n, n, len(bodies[n]), bodies[n])
					case strings.HasPrefix(rest, "LOGOUT"):
						fmt.Fprintf(conn, "* BYE\r\n%s OK bye\r\n", tag)
						return
					}
					fmt.Fprintf(conn, "%s OK done\r\n", tag)
				}
			}(conn)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// The agent's mail is sent and read for real, against servers that speak
// the protocols: a code and a link that come only in an HTML part reach
// it as text.
func TestAnAgentSendsAndReadsItsMail(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	c, _, _ := newCrew(t, &now, nil)
	c.mailPlain = true
	a, _ := c.Create(Agent{Name: "mail", Goal: "g"})
	smtpPort, got := fakeSMTP(t)
	imapPort := fakeIMAP(t)
	ctx := context.Background()
	if err := c.SendMail(ctx, a.ID, Outgoing{To: "x@example.com", Subject: "Hola", Body: "Hi"}); err == nil {
		t.Error("sent with no account in the vault")
	}
	for k, v := range map[string]string{"SMTP_HOST": "127.0.0.1", "SMTP_PORT": strconv.Itoa(smtpPort), "SMTP_USER": "agent@example.com", "SMTP_PASSWORD": `p"w`, "IMAP_PORT": strconv.Itoa(imapPort)} {
		if err := c.SetSecret(a.ID, k, v); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := c.Get(a.ID)
	for _, cp := range st.Capabilities {
		if (cp.Key == "mail-out" || cp.Key == "mail-in") && !cp.Ready {
			t.Errorf("%s not ready with the account in the vault: %v", cp.Key, cp.Missing)
		}
	}
	if err := c.SendMail(ctx, a.ID, Outgoing{To: "Ana <ana@example.com>", Subject: "Qué tal", Body: "Línea uno\nLínea dos"}); err != nil {
		t.Fatal(err)
	}
	msg := <-got
	for _, want := range []string{"From: <agent@example.com>", "To: \"Ana\" <ana@example.com>", "Subject: =?utf-8?q?Qu=C3=A9_tal?=", "L=C3=ADnea uno\r\nL=C3=ADnea dos"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message lacks %q:\n%s", want, msg)
		}
	}
	if err := c.SendMail(ctx, a.ID, Outgoing{To: "x@example.com", Subject: "a\r\nBcc: y@example.com"}); err == nil {
		t.Error("a header smuggled in the subject")
	}
	list, err := c.ListMail(ctx, a.ID, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].UID != 2 || list[0].Subject != "Tu código" || list[0].Seen || !list[1].Seen || list[1].From != "Shop <news@shop.example>" {
		t.Errorf("the inbox: %+v", list)
	}
	if un, err := c.ListMail(ctx, a.ID, true, 10); err != nil || len(un) != 1 || un[0].UID != 2 {
		t.Errorf("unseen: %+v %v", un, err)
	}
	m, err := c.ReadMail(ctx, a.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.Text, "Your code is 482913") || strings.Contains(m.Text, "p{}") || len(m.Links) != 1 || m.Links[0] != "https://service.example/verify?t=abc&x=1" {
		t.Errorf("read: %+v", m)
	}
}
