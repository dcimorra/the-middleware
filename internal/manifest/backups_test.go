package manifest

import (
	"strings"
	"testing"
)

// What a backup may say, and the refusals that name the field: an agent
// writing one learns from the error what to change.
func TestBackupsValidate(t *testing.T) {
	ok := []string{
		`{"version":2,"backups":{"app":{"engine":"sqlite","file":"data/app.db","schedule":"0 3 * * *","timezone":"Europe/Madrid","keep":14}}}`,
		`{"version":2,"workers":{"db":{"command":["mariadbd"],"runtime":"docker","docker":{"image":"mariadb:11.4"}}},"backups":{"db":{"engine":"mysql","service":"db","database":"*"}}}`,
		`{"version":2,"run":{"command":["x"]},"backups":{"pg":{"engine":"postgres","user":"app","password_env":"DB_PASSWORD","host":"127.0.0.1","port":5432}}}`,
		`{"version":2,"backups":{"r":{"engine":"command","dump":["sh","-c","cat x"],"restore":["sh","-c","cat > x"]}}}`,
	}
	for _, s := range ok {
		if _, err := Parse([]byte(s)); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	bad := map[string]string{
		`{"version":2,"backups":{"x":{"engine":"oracle"}}}`:                                                                                                 "backups.x.engine",
		`{"version":2,"backups":{"X":{"engine":"mysql"}}}`:                                                                                                  "is not a name",
		`{"version":2,"backups":{"x":{"engine":"sqlite"}}}`:                                                                                                 "backups.x.file",
		`{"version":2,"backups":{"x":{"engine":"sqlite","file":"../other/app.db"}}}`:                                                                        "backups.x.file",
		`{"version":2,"backups":{"x":{"engine":"mysql","service":"db"}}}`:                                                                                   "backups.x.service",
		`{"version":2,"backups":{"x":{"engine":"mysql","password_env":"hunter2!"}}}`:                                                                        "password_env",
		`{"version":2,"backups":{"x":{"engine":"mysql","database":"-x"}}}`:                                                                                  "backups.x.database",
		`{"version":2,"backups":{"x":{"engine":"mysql","host":"a b"}}}`:                                                                                     "backups.x.host",
		`{"version":2,"backups":{"x":{"engine":"mysql","schedule":"daily"}}}`:                                                                               "backups.x.schedule",
		`{"version":2,"backups":{"x":{"engine":"mysql","keep":-1}}}`:                                                                                        "backups.x.keep",
		`{"version":2,"backups":{"x":{"engine":"command"}}}`:                                                                                                "backups.x.dump",
		`{"version":2,"backups":{"x":{"engine":"mysql","dump":["x"]}}}`:                                                                                     "command engine",
		`{"version":2,"backups":{"x":{"engine":"mysql","password":"s3cret"}}}`:                                                                              "unknown field",
		`{"version":2,"services":{"web":{"run":{"command":["x"]},"domains":["a.test"]},"db":{"run":{"command":["y"]}}},"backups":{"x":{"engine":"mysql"}}}`: "names the one that holds the database",
	}
	for s, want := range bad {
		_, err := Parse([]byte(s))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", s, err, want)
		}
	}
}
