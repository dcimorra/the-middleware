package proxy

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dcimorra/the-middleware/internal/engine"
)

// canaryCookie remembers which side of a canary a visitor landed on, for
// the revision it was drawn for: a page from the new generation that
// loads its assets from the old one breaks on the first renamed file.
const canaryCookie = "__mw_canary"

// forwardCanary sends a request of a site in canary to the side its
// visitor is on, drawing one for a visitor who has none, and counts the
// answer for the engine's judgement.
func (h *Handler) forwardCanary(w http.ResponseWriter, r *http.Request, sn engine.Snapshot) {
	c := sn.Canary
	onCanary, known := canarySide(r, c.Revision)
	if !known {
		onCanary = draw(c.Percent)
		v := "s"
		if onCanary {
			v = "c"
		}
		// Until the canary's end and a little after: a visitor who comes
		// back once it has taken over draws again, for the next one.
		age := int(time.Until(c.Until).Seconds()) + 60
		if age < 60 {
			age = 60
		}
		w.Header().Add("Set-Cookie", (&http.Cookie{Name: canaryCookie, Value: c.Revision + "." + v, Path: "/", MaxAge: age, HttpOnly: true, SameSite: http.SameSiteLaxMode}).String())
	}
	port := sn.Port
	if onCanary {
		port = c.Port
	}
	cw := &codeWriter{ResponseWriter: w}
	h.forward(cw, r, sn, "http://127.0.0.1:"+strconv.Itoa(port))
	if h.NoteCanary != nil && r.Context().Err() == nil {
		h.NoteCanary(sn.Domain, onCanary, cw.code)
	}
}

// canarySide reads the visitor's side for this revision.
func canarySide(r *http.Request, revision string) (canary, known bool) {
	ck, err := r.Cookie(canaryCookie)
	if err != nil {
		return false, false
	}
	rev, side, ok := strings.Cut(ck.Value, ".")
	if !ok || rev != revision || (side != "c" && side != "s") {
		return false, false
	}
	return side == "c", true
}

// draw is true percent times in a hundred.
func draw(percent int) bool {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return binary.LittleEndian.Uint64(b[:])%100 < uint64(percent)
}

// codeWriter keeps the status of an answer and passes the rest through,
// a websocket's hijack and a stream's flushes included.
type codeWriter struct {
	http.ResponseWriter
	code int
}

func (w *codeWriter) WriteHeader(code int) {
	if w.code == 0 && code >= 200 {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *codeWriter) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *codeWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *codeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		w.code = http.StatusSwitchingProtocols
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func (w *codeWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
