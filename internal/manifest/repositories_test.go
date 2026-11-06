package manifest

import (
	"reflect"
	"strings"
	"testing"
)

func TestRepositoriesAreTheDeployOneAndTheListedOnes(t *testing.T) {
	m, err := Parse([]byte(`{"version":2,"static":{},
		"deploy":{"github":{"repository":"dcimorra/luma32-private"}},
		"repositories":[{"repository":"dcimorra/luma32","path":"build/oss/"},{"repository":"DCimorra/Luma32-Private"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Repository{{Repository: "dcimorra/luma32-private"}, {Repository: "dcimorra/luma32", Path: "build/oss"}, {Repository: "DCimorra/Luma32-Private", Path: "."}}
	if got := m.GitRepositories(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	if got := m.GitRepositoryNames(); !reflect.DeepEqual(got, []string{"dcimorra/luma32-private", "dcimorra/luma32"}) {
		t.Fatalf("names %v", got)
	}
	if (&Manifest{}).GitRepositories() != nil {
		t.Fatal("a site without any names repositories")
	}
}

func TestARepositoryOutsideTheSiteOrWithoutAnOwnerIsRefused(t *testing.T) {
	for _, entry := range []string{
		`{"repository":"luma32"}`,
		`{"repository":"a/b/c"}`,
		`{"repository":"git@github.com:a/b"}`,
		`{"repository":"a/b","path":"../other.example"}`,
		`{"repository":"a/b","path":"/srv/x"}`,
	} {
		_, err := Parse([]byte(`{"version":2,"static":{},"repositories":[` + entry + `]}`))
		if err == nil || !strings.Contains(err.Error(), "repositories[0]") {
			t.Fatalf("%s: %v", entry, err)
		}
	}
}
