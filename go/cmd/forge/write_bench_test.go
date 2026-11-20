package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// BenchmarkWriteHTTP mirrors the graded write shape: sequential
// POST /api/v3/repos/o/r/issues on ONE warm keep-alive TCP connection,
// body {"title":"bench issue","body":"body","labels":[]} (compact JSON),
// 201 response read and discarded.
func BenchmarkWriteHTTP(b *testing.B) {
	f := newForge("performance-token", "seed", "", "o")
	repo, failure := f.createRepo("o", record{"name": "r"})
	if failure != nil || repo == nil {
		b.Fatal(failure)
	}
	server := httptest.NewServer(f)
	defer server.Close()
	address := server.Listener.Addr().String()
	conn, err := net.Dial("tcp", address)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)
	body := []byte(`{"title":"bench issue","body":"body","labels":[]}`)
	do := func() {
		header := "POST /api/v3/repos/o/r/issues HTTP/1.1\r\nHost: " + address + "\r\nAuthorization: Bearer performance-token\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n"
		if _, err := io.WriteString(conn, header); err != nil {
			b.Fatal(err)
		}
		if _, err := conn.Write(body); err != nil {
			b.Fatal(err)
		}
		response, err := http.ReadResponse(reader, nil)
		if err != nil {
			b.Fatal(err)
		}
		_, err = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if err != nil || response.StatusCode != http.StatusCreated {
			b.Fatalf("status=%d err=%v", response.StatusCode, err)
		}
	}
	do()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		do()
	}
}
