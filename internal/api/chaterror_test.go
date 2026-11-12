package api

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/dcimorra/the-middleware/internal/agent"
)

// A message its agent's command line cannot carry is the caller's to
// shorten, not the server's failure: a site's code tried a 500 again
// every few seconds, each one an api.error, for an answer that could not
// change.
func TestAMessageTooLongForTheAgentIsA413(t *testing.T) {
	err := chatError(fmt.Errorf("grok: an argument of 300 KiB: %w", agent.ErrTooLong))
	if status, _ := statusOf(err); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", status)
	}
}
