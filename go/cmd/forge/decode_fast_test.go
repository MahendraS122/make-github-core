package main

import (
	"encoding/json"
	"testing"
)

func decodeBoth(t *testing.T, body string) (*issueInput, *issueInput) {
	t.Helper()
	fast := &issueInput{}
	if !tryDecodeIssue([]byte(body), fast) {
		t.Fatalf("fast path rejected %q", body)
	}
	slow := &issueInput{}
	if err := json.Unmarshal([]byte(body), slow); err != nil {
		t.Fatalf("unmarshal rejected %q: %v", body, err)
	}
	return fast, slow
}

func TestTryDecodeIssueMatchesEncodingJSON(t *testing.T) {
	bodies := []string{
		`{"title":"bench issue","body":"body","labels":[]}`,
		`{"title":"a"}`,
		`{"body":"b"}`,
		`{}`,
		`{"title":"x","body":"y","labels":["bug","help"]}`,
		`{"labels":["a"],"title":"t","body":"b"}`,
		`{"title":"","body":"","labels":[]}`,
		`{"title":"héllo wörld","body":"日本語テキスト","labels":[]}`,
		`{"title":"a","title":"b"}`,
		`{"title":"a"} `,
		` {"title":"a"}`,
	}
	for _, body := range bodies {
		fast, slow := decodeBoth(t, body)
		fastJSON, _ := json.Marshal(fast)
		slowJSON, _ := json.Marshal(slow)
		if string(fastJSON) != string(slowJSON) {
			t.Errorf("mismatch for %q:\nfast %s\nslow %s", body, fastJSON, slowJSON)
		}
	}
}

func TestTryDecodeIssueRejects(t *testing.T) {
	rejected := []string{
		`null`,
		``,
		`[]`,
		`"str"`,
		`{"title":"a",}`,
		`{"Title":"case"}`,
		`{"title":"esc\naped"}`,
		`{"title":"bad\q"}`,
		`{"title":"quote\"inside"}`,
		`{"unknown":"x"}`,
		`{"title":5}`,
		`{"title":null}`,
		`{"title":true}`,
		`{"labels":[1]}`,
		`{"labels":["a",]}`,
		`{"labels":{"name":"a"}}`,
		`{"title":"a"}trailing`,
		`{"title":"a"`,
		"{\"title\":\"control\x01char\"}",
	}
	for _, body := range rejected {
		if tryDecodeIssue([]byte(body), &issueInput{}) {
			t.Errorf("fast path wrongly accepted %q", body)
		}
	}
}
