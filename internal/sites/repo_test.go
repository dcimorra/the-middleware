package sites

import "testing"

func TestARepositoryIsReadFromWhatAPersonPastes(t *testing.T) {
	for in, want := range map[string]string{
		"octocat/Spoon-Knife":                          "octocat/Spoon-Knife",
		"https://github.com/octocat/Spoon-Knife":       "octocat/Spoon-Knife",
		"https://github.com/octocat/Spoon-Knife.git":   "octocat/Spoon-Knife",
		"github.com/octocat/Spoon-Knife/tree/main/src": "octocat/Spoon-Knife",
		"git@github.com:octocat/Spoon-Knife.git":       "octocat/Spoon-Knife",
	} {
		if got, err := ParseRepo(in); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "nope", "https://gitlab.com/a/b", "a/../b", "a/b/c"} {
		if _, err := ParseRepo(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTheRepositoryRootDecidesWhatTheSiteIs(t *testing.T) {
	node := Repo{FullName: "a/app", DefaultBranch: "main", Files: []string{"package.json", "package-lock.json"}, PackageJSON: []byte(`{"scripts":{"start":"node s.js","build":"tsc"}}`)}
	m, kind := RepoManifest(node, "", "")
	if kind != "node" || m.Run == nil || m.Deploy.GitHub.Branch != "main" || len(m.Deploy.GitHub.Build) != 2 || m.Deploy.GitHub.Build[0][1] != "ci" {
		t.Fatalf("node: %s %+v", kind, m.Deploy.GitHub)
	}
	vite := Repo{FullName: "a/web", Files: []string{"package.json"}, PackageJSON: []byte(`{"scripts":{"build":"vite build"}}`)}
	if m, kind := RepoManifest(vite, "dev", ""); kind != "static-build" || m.Static.Root != "dist" || m.Deploy.GitHub.Branch != "dev" {
		t.Fatalf("vite: %s %+v", kind, m)
	}
	next := Repo{FullName: "a/n", Files: []string{"package.json"}, PackageJSON: []byte(`{"scripts":{"build":"next build"}}`)}
	if m, kind := RepoManifest(next, "", ""); kind != "static-build" || m.Static.Root != "out" {
		t.Fatalf("next export: %s %+v", kind, m.Static)
	}
	if _, kind := RepoManifest(Repo{FullName: "a/g", Files: []string{"go.mod", "main.go"}}, "", ""); kind != "go" {
		t.Fatalf("go: %s", kind)
	}
	if m, kind := RepoManifest(Repo{FullName: "a/s", Files: []string{"index.html"}}, "", ""); kind != "static" || m.Static.Root != "" {
		t.Fatalf("static: %s %+v", kind, m.Static)
	}
	// The operator's choice wins over the guess.
	if _, kind := RepoManifest(node, "", "go"); kind != "go" {
		t.Fatalf("template ignored: %s", kind)
	}
	if err := func() error { m, _ := RepoManifest(node, "", ""); return m.Validate() }(); err != nil {
		t.Fatal(err)
	}
}
