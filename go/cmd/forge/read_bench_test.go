package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func BenchmarkReadHTTP(b *testing.B) {
	f := newForge("performance-token", "seed", "", "o")
	repo, failure := f.createRepo("o", record{"name": "r"})
	if failure != nil {
		b.Fatal(failure)
	}
	for index := 0; index < 25; index++ {
		if _, failure := f.newIssue(repo, record{"title": "initial issue", "body": "body"}); failure != nil {
			b.Fatal(failure)
		}
	}
	server := httptest.NewServer(f)
	defer server.Close()
	transport := &http.Transport{MaxIdleConns: 1, MaxIdleConnsPerHost: 1}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	request, err := http.NewRequest(http.MethodGet, server.URL+"/repos/o/r/issues", nil)
	if err != nil {
		b.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer performance-token")
	if response, err := client.Do(request); err != nil {
		b.Fatal(err)
	} else {
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			b.Fatalf("status=%d", response.StatusCode)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		request, err := http.NewRequest(http.MethodGet, server.URL+"/repos/o/r/issues", nil)
		if err != nil {
			b.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer performance-token")
		response, err := client.Do(request)
		if err != nil {
			b.Fatal(err)
		}
		_, err = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK {
			b.Fatalf("status=%d err=%v", response.StatusCode, err)
		}
	}
}
