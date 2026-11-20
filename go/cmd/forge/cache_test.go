package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

func TestIssueEncodingAndInvalidation(t *testing.T) {
	f := newForge("token", "seed", "2025-01-01T00:00:00Z", "o")
	repo, failure := f.createRepo("o", record{"name": "r"})
	if failure != nil {
		t.Fatal(failure)
	}
	for _, title := range []string{"plain", "quote\"\\\n\t<>&", "日本語\u2028\u2029", string([]byte{255})} {
		item, failure := f.newIssue(repo, record{"title": title, "body": title})
		if failure != nil {
			t.Fatal(failure)
		}
		number := strconv.Itoa(item.number)
		path := repoURL(repo) + "/issues/" + number
		expected, _ := json.Marshal(issueJSON{
			ID: item.id, NodeID: node("Issue", item.id), Number: item.number, Title: title, Body: title,
			State: "open", User: f.viewerJSON, Labels: []record{}, Assignees: []record{},
			CreatedAt: f.timestamp, UpdatedAt: f.timestamp, URL: path, RepositoryURL: repoURL(repo),
			CommentsURL: path + "/comments", LabelsURL: path + "/labels{/name}",
			HTMLURL: "/o/r/issues/" + number, AuthorAssociation: "OWNER",
		})
		if !bytes.Equal(item.cached, expected) {
			t.Fatalf("encoding mismatch:\n%s\n%s", item.cached, expected)
		}
	}
	req := httptest.NewRequest("GET", "/repos/o/r/issues?per_page=2", nil)
	cached := repo.cachedIssueList(req)
	if cached.next != "page=2&per_page=2" || cached != repo.cachedIssueList(req) {
		t.Fatal("list cache or pagination mismatch")
	}
	item := repo.issues[4]
	item.state = "closed"
	f.cacheIssue(repo, item)
	if repo.cachedIssueList(req) == cached || repo.openIssues != 3 {
		t.Fatal("list or repository count not invalidated")
	}
	if expected, _ := json.Marshal(f.repoView(repo)); !bytes.Equal(repo.cached, expected) {
		t.Fatal("repository cache mismatch")
	}
	f.comment(repo, repo.issues[1], "comment")
	if !bytes.Contains(repo.issues[1].cached, []byte(`"comments":1`)) {
		t.Fatal("comment count not invalidated")
	}
	for _, query := range []string{"page=999", "state=closed", "state=all&direction=asc", "per_page=-1", "page=9223372036854775807"} {
		cached := repo.cachedIssueList(httptest.NewRequest("GET", "/repos/o/r/issues?"+query, nil))
		if !json.Valid(cached.body) {
			t.Fatal("invalid list JSON")
		}
	}
}

func TestConcurrentPackCache(t *testing.T) {
	f := newForge("token", "seed", "", "o")
	repo, _ := f.createRepo("o", record{"name": "r", "auto_init": true})
	body := shallowRequest(repo.refs[repo.head()])
	var group sync.WaitGroup
	responses := make(chan []byte, 50)
	for index := 0; index < 50; index++ {
		group.Go(func() {
			repo.guard.RLock()
			responses <- repo.upload(body, false)
			repo.guard.RUnlock()
		})
	}
	group.Wait()
	close(responses)
	var first []byte
	for response := range responses {
		if first == nil {
			first = response
		}
		if !bytes.Equal(first, response) || &first[0] != &response[0] {
			t.Fatal("concurrent clones did not share the cached pack")
		}
	}
	repo.guard.Lock()
	repo.changed()
	repo.guard.Unlock()
	repo.guard.RLock()
	next := repo.upload(body, false)
	repo.guard.RUnlock()
	if !bytes.Equal(first, next) || &first[0] == &next[0] {
		t.Fatal("pack invalidation mismatch")
	}
}

func TestConcurrentReceive(t *testing.T) {
	f := newForge("token", "seed", "", "o")
	repo, _ := f.createRepo("o", record{"name": "r"})
	bodies, heads := performancePushes(1)
	var group sync.WaitGroup
	responses := make(chan []byte, 20)
	for index := 0; index < 20; index++ {
		group.Go(func() { responses <- repo.receive(bodies[0]) })
	}
	group.Wait()
	close(responses)
	accepted := 0
	for response := range responses {
		if bytes.Contains(response, []byte("ok refs/heads/main")) {
			accepted++
		} else if !bytes.Contains(response, []byte("stale ref")) {
			t.Fatalf("unexpected receive response: %s", response)
		}
	}
	if accepted != 1 || repo.refs[repo.head()] != heads[0] || len(repo.objects) != 3 {
		t.Fatal("receive did not atomically validate and update refs")
	}
}

func BenchmarkCachedIssueList(b *testing.B) {
	f := newForge("token", "seed", "", "o")
	repo, _ := f.createRepo("o", record{"name": "r"})
	for index := 0; index < 25; index++ {
		f.newIssue(repo, record{"title": "issue", "body": "body"})
	}
	req := httptest.NewRequest("GET", "/repos/o/r/issues", nil)
	repo.cachedIssueList(req)
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		repo.guard.RLock()
		repo.cachedIssueList(req)
		repo.guard.RUnlock()
	}
}
