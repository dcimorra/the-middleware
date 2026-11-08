package api

import (
	"errors"
	"net/http"

	"github.com/dcimorra/the-middleware/internal/backups"
)

// registerDatabases is the database browser: the site's databases, their
// tables, and a read-only query, run where the database is
// (internal/backups, query.go). It is in the code permission, with exec:
// it shows what the site keeps, its customers' data among it.
func (s *Server) registerDatabases(k *backups.Keeper) {
	base := Prefix + "/sites/{domain}/databases"
	s.Handle("GET", base, "The site's databases: the ones its backups name (SQLite, PostgreSQL, MySQL) and the SQLite files in its folder no backup names", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		list, err := k.Databases(domain)
		if err != nil {
			return nil, backupErr(err)
		}
		if list == nil {
			list = []backups.Database{}
		}
		return list, nil
	})
	s.Handle("GET", base+"/tables", "One database's tables and views (?database=name)", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		res, err := k.Tables(r.Context(), domain, r.URL.Query().Get("database"))
		return res, queryErr(res, err)
	})
	s.Handle("POST", base+"/query", "Run one read-only query: {database, sql} → {columns, rows, truncated, ms}, at most 1000 rows; a write is refused by the database", func(w http.ResponseWriter, r *http.Request) (any, error) {
		domain, err := domainOf(r)
		if err != nil {
			return nil, err
		}
		var body struct {
			Database string `json:"database"`
			SQL      string `json:"sql"`
		}
		if err := ReadJSON(r, &body); err != nil {
			return nil, err
		}
		res, err := k.Query(r.Context(), domain, body.Database, body.SQL)
		return res, queryErr(res, err)
	})
}

func queryErr(_ backups.QueryResult, err error) error {
	if err == nil {
		return nil
	}
	var qe *backups.QueryError
	switch {
	case errors.As(err, &qe):
		return Errorf(http.StatusUnprocessableEntity, "%s", qe.Message)
	case errors.Is(err, backups.ErrNoDatabase):
		return Errorf(http.StatusNotFound, "%v", err)
	}
	return backupErr(err)
}
