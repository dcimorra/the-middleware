package api

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/events"
	"github.com/dcimorra/the-middleware/internal/sitetoken"
)

// The git proxy is how a site's conversation pushes with the GitHub
// module's token without ever holding it. A credential helper was the
// first idea and was dropped before it was written: whatever a helper
// hands git, the agent can read with `git credential fill`, and a
// fine-grained token with write access usually reaches every repository
// of its owner, so "limited to the site's repository" would have been a
// promise kept by the agent's good manners. Here the turn carries a token
// that only this proxy accepts, git is told (through GIT_CONFIG_* in the
// turn's environment, no file) to send the site's repositories here, and
// the house adds GitHub's credential after checking the repository is one
// the site's manifest names: the one it deploys from and those under
// `repositories` (a public copy kept in build/oss, say).

// GitPrefix is where the proxy answers: GitPrefix/<owner>/<name>/…
const GitPrefix = Prefix + "/site/git"

// GitProxyOptions wire the git proxy.
type GitProxyOptions struct {
	Tokens *sitetoken.Keys
	// Repositories are the owner/name a site may reach (its manifest's
	// deploy.github.repository and repositories), none when it declares
	// none.
	Repositories func(domain string) []string
	// Token is the GitHub module's token, "" when the module is off or
	// has none. Read per request, so a rotated token is used at once.
	Token func() string
	// Upstream is https://github.com; tests point it at a git http-backend.
	Upstream string
	HTTP     *http.Client
	Bus      *events.Bus
}

// GitEnv is what a conversation's turn needs for git to reach repos
// through the proxy at api (http://127.0.0.1:port): the usual remote
// addresses of each repository are rewritten to the proxy, and the
// proxy's token goes as a header only to the proxy. Any other remote is
// left alone.
func GitEnv(api string, repos []string, token string) map[string]string {
	base := strings.TrimSuffix(api, "/") + GitPrefix + "/"
	var pairs [][2]string
	for _, repo := range repos {
		pairs = append(pairs,
			[2]string{"url." + base + repo + ".insteadOf", "https://github.com/" + repo},
			[2]string{"url." + base + repo + ".insteadOf", "git@github.com:" + repo},
			[2]string{"url." + base + repo + ".insteadOf", "ssh://git@github.com/" + repo})
	}
	pairs = append(pairs, [2]string{"http." + base + ".extraHeader", "Authorization: Bearer " + token})
	env := map[string]string{"GIT_CONFIG_COUNT": strconv.Itoa(len(pairs))}
	for i, p := range pairs {
		env["GIT_CONFIG_KEY_"+strconv.Itoa(i)] = p[0]
		env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)] = p[1]
	}
	return env
}

// RegisterGitProxy adds the smart-HTTP routes git uses, and only those:
// the dumb protocol and anything else under the prefix is a 404.
func (s *Server) RegisterGitProxy(o GitProxyOptions) {
	if o.Upstream == "" {
		o.Upstream = "https://github.com"
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{}
	}
	if o.Bus == nil {
		o.Bus = events.New("")
	}
	p := GitPrefix + "/{owner}/{repo}"
	for _, rt := range []struct{ method, op, summary string }{
		{"GET", "info/refs", "Git's ref advertisement for the site's repository, with the GitHub module's token (a conversation's push token)"},
		{"POST", "git-upload-pack", "Git fetch from the site's repository, with the GitHub module's token"},
		{"POST", "git-receive-pack", "Git push to the site's repository, with the GitHub module's token; deleting a branch or tag is refused"},
	} {
		op := rt.op
		s.Handle(rt.method, p+"/"+op, rt.summary, func(w http.ResponseWriter, r *http.Request) (any, error) {
			return o.serve(w, r, op)
		})
		s.Public(rt.method, p+"/"+op)
	}
}

func (o GitProxyOptions) serve(w http.ResponseWriter, r *http.Request, op string) (any, error) {
	domain := ""
	if tok, ok := strings.CutPrefix(strings.TrimSpace(r.Header.Get("Authorization")), "Bearer "); ok && o.Tokens != nil {
		domain = o.Tokens.GitDomain(strings.TrimSpace(tok))
	}
	if domain == "" {
		time.Sleep(400 * time.Millisecond)
		// 403 and not 401: a 401 makes git ask for a password, and there
		// is nobody at a terminal to answer it.
		return nil, Errorf(http.StatusForbidden, "a conversation's push token is required")
	}
	refuse := func(format string, args ...any) (any, error) {
		msg := fmt.Sprintf(format, args...)
		o.Bus.Emit(events.Event{Type: "git.refused", Subject: domain, Actor: "agent", Level: "warn", Message: msg})
		return nil, Errorf(http.StatusForbidden, "%s", msg)
	}
	var repos []string
	if o.Repositories != nil {
		repos = o.Repositories(domain)
	}
	asked := r.PathValue("owner") + "/" + strings.TrimSuffix(r.PathValue("repo"), ".git")
	if len(repos) == 0 {
		return refuse("%s declares no deploy.github.repository nor repositories in its manifest, so it has no repository to push to", domain)
	}
	// GitHub's names are not case-sensitive; the manifest and the remote
	// may well be written differently.
	repo := ""
	for _, x := range repos {
		if strings.EqualFold(asked, x) {
			repo = x
		}
	}
	if repo == "" {
		return refuse("%s may only reach %s through the house, not %s; another repository of the site goes in the manifest's repositories", domain, strings.Join(repos, ", "), asked)
	}
	token := ""
	if o.Token != nil {
		token = o.Token()
	}
	if token == "" {
		return refuse("the GitHub module is off or has no access token")
	}
	if op == "info/refs" {
		if svc := r.URL.Query().Get("service"); svc != "git-upload-pack" && svc != "git-receive-pack" {
			return nil, Errorf(http.StatusNotFound, "only git's smart protocol is proxied")
		}
	}
	body := io.Reader(r.Body)
	var refs []string
	if op == "git-receive-pack" {
		if r.Header.Get("Content-Encoding") != "" {
			return refuse("a compressed push cannot be checked")
		}
		cmds, head, err := readPushCommands(r.Body)
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "push: %v", err)
		}
		for _, c := range cmds {
			if c.deletes() {
				// The jail already refuses `git push --delete`; this is
				// the same rule where it cannot be worded around.
				return refuse("%s: deleting %s is not allowed from a conversation", domain, c.ref)
			}
			refs = append(refs, c.ref)
		}
		body = io.MultiReader(bytes.NewReader(head), r.Body)
	}
	// The name as the remote wrote it: equal to the manifest's but for
	// case, which GitHub ignores and a plain git server does not.
	url := strings.TrimSuffix(o.Upstream, "/") + "/" + asked + ".git/" + op
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, url, body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = r.ContentLength
	for _, h := range []string{"Content-Type", "Content-Encoding", "Accept", "Accept-Encoding", "Git-Protocol", "User-Agent"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	req.SetBasicAuth("x-access-token", token)
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return nil, Errorf(http.StatusBadGateway, "github: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		return refuse("GitHub answered %s for %s with the module's token: it needs Contents read and write on that repository", resp.Status, repo)
	}
	for _, h := range []string{"Content-Type", "Content-Encoding", "Cache-Control", "Expires", "Pragma"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, err = io.Copy(w, resp.Body)
	if op == "git-receive-pack" && err == nil && resp.StatusCode < 300 {
		o.Bus.Emit(events.Event{Type: "git.pushed", Subject: domain, Actor: "agent", Message: repo + " " + strings.Join(refs, " "), Data: map[string]any{"repository": repo, "refs": refs}})
	}
	return Handled, nil
}

// pushCommand is one line of a push: a ref going from old to new.
type pushCommand struct {
	old, new, ref string
}

func (c pushCommand) deletes() bool { return strings.Trim(c.new, "0") == "" }

// maxPushHead bounds the command list; a push of thousands of refs is a
// few hundred KB.
const maxPushHead = 4 << 20

// readPushCommands reads the pkt-lines before the pack, up to the flush,
// and returns them with the bytes read, which go back in front of the
// rest of the body unchanged.
func readPushCommands(body io.Reader) ([]pushCommand, []byte, error) {
	var head bytes.Buffer
	br := bufio.NewReader(io.LimitReader(body, maxPushHead))
	var cmds []pushCommand
	for {
		var size [4]byte
		if _, err := io.ReadFull(br, size[:]); err != nil {
			return nil, nil, fmt.Errorf("reading the commands: %w", err)
		}
		head.Write(size[:])
		n, err := strconv.ParseUint(string(size[:]), 16, 16)
		if err != nil {
			return nil, nil, errors.New("not a pkt-line")
		}
		if n == 0 {
			break
		}
		if n < 4 {
			return nil, nil, errors.New("not a pkt-line")
		}
		line := make([]byte, n-4)
		if _, err := io.ReadFull(br, line); err != nil {
			return nil, nil, fmt.Errorf("reading the commands: %w", err)
		}
		head.Write(line)
		text, _, _ := strings.Cut(strings.TrimSuffix(string(line), "\n"), "\x00")
		f := strings.Fields(text)
		if len(f) == 3 && len(f[0]) >= 40 && len(f[1]) >= 40 {
			cmds = append(cmds, pushCommand{old: f[0], new: f[1], ref: f[2]})
		}
	}
	// What the bufio reader took past the flush is the start of the pack.
	rest, _ := br.Peek(br.Buffered())
	head.Write(rest)
	return cmds, head.Bytes(), nil
}
