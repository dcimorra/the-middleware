package manifest

import "testing"

func TestGuestsAreSitesToReadOrToWrite(t *testing.T) {
	m, err := Parse([]byte(`{"version":2,"static":{},"guests":{"new.example.com":"read","tools":"write"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := GuestNames(m.Guests); len(got) != 2 || got[0] != "new.example.com" || got[1] != "tools" {
		t.Fatalf("names %v", got)
	}
	for _, bad := range []string{
		`{"New.Example.com":"read"}`,
		`{"../other":"read"}`,
		`{"new.example.com":"all"}`,
		`{"new.example.com":""}`,
	} {
		if _, err := Parse([]byte(`{"version":2,"static":{},"guests":` + bad + `}`)); err == nil {
			t.Errorf("%s was taken", bad)
		}
	}
}
