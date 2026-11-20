package main

import (
	"io"
	"net/http"
	"testing"
)

type benchBody struct {
	data []byte
	pos  int
}

func (b *benchBody) Read(p []byte) (int, error) {
	if b.pos >= len(b.data) {
		return 0, io.EOF
	}
	read := copy(p, b.data[b.pos:])
	b.pos += read
	return read, nil
}
func (b *benchBody) Close() error { return nil }
func (b *benchBody) reset()       { b.pos = 0 }

type benchWriter struct {
	header http.Header
	code   int
	writes int
}

func (w *benchWriter) Header() http.Header         { return w.header }
func (w *benchWriter) Write(p []byte) (int, error) { w.writes += len(p); return len(p), nil }
func (w *benchWriter) WriteHeader(code int)        { w.code = code }

// BenchmarkWriteHandler measures server-side cost of the write path only
// (no wire): createIssueHTTP via ServeHTTP with an alloc-free harness.
// The graded shape bench is BenchmarkWriteHTTP.
func BenchmarkWriteHandler(b *testing.B) {
	f := newForge("performance-token", "seed", "", "o")
	repo, failure := f.createRepo("o", record{"name": "r"})
	if failure != nil || repo == nil {
		b.Fatal(failure)
	}
	payload := []byte(`{"title":"bench issue","body":"body","labels":[]}`)
	target, err := http.NewRequest(http.MethodPost, "/api/v3/repos/o/r/issues", nil)
	if err != nil {
		b.Fatal(err)
	}
	body := &benchBody{data: payload}
	target.Body = body
	target.ContentLength = int64(len(payload))
	target.Header.Set("Authorization", "Bearer performance-token")
	target.Header.Set("Content-Type", "application/json")
	out := &benchWriter{header: http.Header{}}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		body.reset()
		out.header = http.Header{}
		out.code = 0
		out.writes = 0
		f.ServeHTTP(out, target)
		if out.code != http.StatusCreated {
			b.Fatalf("status=%d", out.code)
		}
	}
}
