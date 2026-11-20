package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func BenchmarkCloneHTTP(b *testing.B) {
	f := newForge("performance-token", "seed", "", "o")
	repo, _ := f.createRepo("o", record{"name": "r"})
	bodies, heads := performancePushes(200)
	for _, body := range bodies {
		if !bytes.Contains(repo.receive(body), []byte("ok refs/heads/main")) {
			b.Fatal("fixture push failed")
		}
	}
	server := httptest.NewServer(f)
	defer server.Close()
	transport := &http.Transport{MaxIdleConns: 100, MaxIdleConnsPerHost: 100}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	refs := packet(nil, "command=ls-refs\n")
	refs = packet(refs, "agent=makefun-git/1\n")
	refs = packet(refs, "object-format=sha1\n")
	refs = append(refs, "0001"...)
	for _, line := range []string{"peel\n", "symrefs\n", "unborn\n"} {
		refs = packet(refs, line)
	}
	refs = append(refs, "0000"...)
	fetch := packet(nil, "command=fetch\n")
	fetch = packet(fetch, "agent=makefun-git/1\n")
	fetch = packet(fetch, "object-format=sha1\n")
	fetch = append(fetch, "0001"...)
	for _, line := range []string{"thin-pack\n", "no-progress\n", "ofs-delta\n", "want " + heads[len(heads)-1] + "\n", "deepen 1\n", "done\n"} {
		fetch = packet(fetch, line)
	}
	fetch = append(fetch, "0000"...)
	clone := func() error {
		for _, body := range [][]byte{nil, nil, refs, nil, fetch} {
			method, path := "POST", "/o/r.git/git-upload-pack"
			if body == nil {
				method, path = "GET", "/o/r.git/info/refs?service=git-upload-pack"
			}
			req, err := http.NewRequest(method, server.URL+path, bytes.NewReader(body))
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer performance-token")
			req.Header.Set("Git-Protocol", "version=2")
			response, err := client.Do(req)
			if err != nil {
				return err
			}
			_, err = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != 200 {
				return fmt.Errorf("clone status=%d err=%v", response.StatusCode, err)
			}
		}
		return nil
	}
	if err := clone(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		var group sync.WaitGroup
		failures := make(chan error, 50)
		for worker := 0; worker < 50; worker++ {
			group.Go(func() {
				if err := clone(); err != nil {
					failures <- err
				}
			})
		}
		group.Wait()
		close(failures)
		for err := range failures {
			b.Fatal(err)
		}
	}
}
