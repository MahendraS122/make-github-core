package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestForgeProcess(t *testing.T) {
	if os.Getenv("FORGE_TEST_PROCESS") == "1" {
		main()
		os.Exit(0)
	}
}

func startTestForge(t *testing.T) (string, *http.Client) {
	t.Helper()
	registered := make(chan int, 1)
	stub := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
		out.Header().Set("Content-Type", "application/json")
		if req.URL.Path == "/register" && req.Method == "POST" {
			var registration struct{ Port int }
			if err := json.NewDecoder(req.Body).Decode(&registration); err != nil {
				t.Error(err)
			}
			registered <- registration.Port
			out.Write([]byte(`{"ok":true}`))
			return
		}
		if req.URL.Path == "/done" {
			out.Write([]byte(`{"done":false}`))
			return
		}
		out.WriteHeader(404)
	}))
	t.Cleanup(stub.Close)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"-test.run=^TestForgeProcess$"}
	if binary := os.Getenv("FORGE_TEST_BINARY"); binary != "" {
		executable, args = binary, nil
	}
	process := exec.Command(executable, args...)
	login := "o"
	if os.Getenv("FORGE_CONFORMANCE_RUNNER") != "" {
		login = "adaiversen"
	}
	process.Env = append(os.Environ(), "FORGE_TEST_PROCESS=1", "MAKEFUN_ENV_URL="+stub.URL,
		"MAKEFUN_SHOP_TOKEN=performance-token", "MAKEFUN_TRIAL_SEED=performance-seed",
		"MAKEFUN_TRIAL_NOW=2025-01-01T00:00:00Z", "MAKEFUN_FORGE_LOGIN="+login, "GOMAXPROCS=2")
	var logs bytes.Buffer
	process.Stdout, process.Stderr = &logs, &logs
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		process.Process.Kill()
		process.Wait()
		if t.Failed() {
			t.Log(logs.String())
		}
	})
	transport := &http.Transport{MaxIdleConns: 100, MaxIdleConnsPerHost: 100}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	select {
	case port := <-registered:
		return "http://127.0.0.1:" + strconv.Itoa(port), client
	case <-time.After(10 * time.Second):
		t.Fatal("forge failed to register")
		return "", nil
	}
}

func testRequest(client *http.Client, origin, method, path string, body []byte) ([]byte, int, error) {
	req, err := http.NewRequest(method, origin+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer performance-token")
	response, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	var data []byte
	if response.ContentLength >= 0 {
		data = make([]byte, response.ContentLength)
		_, err = io.ReadFull(response.Body, data)
	} else {
		data, err = io.ReadAll(response.Body)
	}
	return data, response.StatusCode, err
}

func requireRequest(t *testing.T, client *http.Client, origin, method, path string, body []byte, status int) []byte {
	t.Helper()
	data, got, err := testRequest(client, origin, method, path, body)
	if err != nil || got != status {
		t.Fatalf("%s %s: status=%d err=%v body=%s", method, path, got, err, data)
	}
	return data
}

func performancePushes(count int) ([][]byte, []string) {
	bodies := make([][]byte, count)
	heads := make([]string, count)
	parent := zero
	for index := range bodies {
		blob := newObject(3, bytes.Repeat([]byte{byte(index), byte(index >> 8), 'x', 'y'}, 256), nil)
		blobID := objectID(3, blob.data)
		store := objects{blobID: blob}
		treeID := store.makeTree(map[string]treeEntry{"blob": {"100644", "blob", blobID}})
		commitData := "tree " + treeID + "\n"
		if parent != zero {
			commitData += "parent " + parent + "\n"
		}
		commitData += "author Bench <bench@example.test> 1735689600 +0000\ncommitter Bench <bench@example.test> 1735689600 +0000\n\ncommit " + strconv.Itoa(index) + "\n"
		commit := newObject(1, []byte(commitData), nil)
		heads[index] = objectID(1, commit.data)
		body := packet(nil, parent+" "+heads[index]+" refs/heads/main\x00report-status\n")
		body = append(body, "0000"...)
		bodies[index] = append(body, writePack([]*object{commit, store[treeID], blob}, false)...)
		parent = heads[index]
	}
	return bodies, heads
}

func shallowRequest(head string) []byte {
	body := packet(nil, "want "+head+" side-band-64k ofs-delta\n")
	body = packet(body, "deepen 1\n")
	body = append(body, "0000"...)
	return packet(body, "done\n")
}

func validateShallowPack(t *testing.T, data []byte, head string) {
	t.Helper()
	var pack []byte
	for len(data) > 0 {
		line, length, rest := unpackLine(data)
		if length < 0 {
			t.Fatal("invalid upload response")
		}
		data = rest
		if len(line) > 0 && line[0] == 1 {
			pack = append(pack, line[1:]...)
		}
	}
	store, err := readPack(pack, nil)
	if err != nil || len(store) != 3 || store[head] == nil {
		t.Fatalf("invalid shallow pack: objects=%d err=%v", len(store), err)
	}
}

func sequentialRequests(t *testing.T, origin, method, path string, body []byte, count, status int) time.Duration {
	t.Helper()
	connection, err := net.Dial("tcp4", strings.TrimPrefix(origin, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(30 * time.Second))
	reader := bufio.NewReader(connection)
	wire := []byte(method + " " + path + " HTTP/1.1\r\nHost: " + strings.TrimPrefix(origin, "http://") +
		"\r\nAuthorization: Bearer performance-token\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n")
	wire = append(wire, body...)
	request := &http.Request{Method: method}
	started := time.Now()
	for index := 0; index < count; index++ {
		if _, err := connection.Write(wire); err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(reader, request)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != status {
			t.Fatalf("unexpected status: %d", response.StatusCode)
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
	}
	return time.Since(started)
}

func TestPerformance(t *testing.T) {
	if os.Getenv("FORGE_PERF") != "1" {
		t.Skip("set FORGE_PERF=1 for subprocess HTTP benchmarks")
	}
	origin, client := startTestForge(t)
	requireRequest(t, client, origin, "POST", "/orgs/o/repos", []byte(`{"name":"r"}`), 201)
	for index := 0; index < 25; index++ {
		requireRequest(t, client, origin, "POST", "/repos/o/r/issues", []byte(`{"title":"initial issue","body":"body"}`), 201)
	}
	requireRequest(t, client, origin, "GET", "/repos/o/r/issues", nil, 200)
	readTime := sequentialRequests(t, origin, "GET", "/repos/o/r/issues", nil, 2000, 200)
	writeTime := sequentialRequests(t, origin, "POST", "/repos/o/r/issues", []byte(`{"title":"created issue","body":"body"}`), 200, 201)
	t.Logf("read 2000: %s (%d ns/op); write 200: %s (%d ns/op)", readTime, readTime.Nanoseconds()/2000, writeTime, writeTime.Nanoseconds()/200)
	bodies, heads := performancePushes(200)
	var started time.Time
	var pushTimes []time.Duration
	for round := 0; round < 3; round++ {
		name := "git" + strconv.Itoa(round)
		requireRequest(t, client, origin, "POST", "/orgs/o/repos", []byte(`{"name":"`+name+`"}`), 201)
		started = time.Now()
		for index, body := range bodies {
			response := requireRequest(t, client, origin, "POST", "/o/"+name+".git/git-receive-pack", body, 200)
			if !bytes.Contains(response, []byte("unpack ok")) || !bytes.Contains(response, []byte("ok refs/heads/main")) {
				t.Fatalf("push rejected: %s", response)
			}
			if index == 2 {
				t.Logf("push round %d first 3: %s", round+1, time.Since(started))
			}
		}
		pushTimes = append(pushTimes, time.Since(started))
	}
	t.Logf("push 200 rounds: %v", pushTimes)
	body := shallowRequest(heads[len(heads)-1])
	for _, count := range []int{10, 50} {
		repository := "git0"
		if count == 50 {
			repository = "git1"
		}
		var group sync.WaitGroup
		errors := make(chan error, count)
		responses := make(chan []byte, count)
		started = time.Now()
		for index := 0; index < count; index++ {
			group.Go(func() {
				response, status, err := testRequest(client, origin, "POST", "/o/"+repository+".git/git-upload-pack", body)
				if err != nil || status != 200 {
					errors <- fmt.Errorf("clone status=%d err=%v", status, err)
					return
				}
				responses <- response
			})
		}
		group.Wait()
		elapsed := time.Since(started)
		close(errors)
		close(responses)
		for err := range errors {
			t.Error(err)
		}
		for response := range responses {
			validateShallowPack(t, response, heads[len(heads)-1])
		}
		t.Logf("clone %d concurrent depth-1: %s", count, elapsed)
		if os.Getenv("FORGE_PERF_ENFORCE") == "1" && count == 50 && elapsed >= 150*time.Millisecond {
			t.Errorf("clone target missed: %s", elapsed)
		}
	}
	if os.Getenv("FORGE_PERF_ENFORCE") == "1" {
		if readTime >= 1600*time.Millisecond || writeTime >= 200*time.Millisecond {
			t.Errorf("API targets missed: read=%s write=%s", readTime, writeTime)
		}
		for _, elapsed := range pushTimes {
			if elapsed >= 950*time.Millisecond {
				t.Errorf("push target missed: %s", elapsed)
			}
		}
	}
}

func TestExternalConformance(t *testing.T) {
	runner := os.Getenv("FORGE_CONFORMANCE_RUNNER")
	if runner == "" {
		t.Skip("set FORGE_CONFORMANCE_RUNNER to the SDK run.ts path")
	}
	origin, _ := startTestForge(t)
	args := []string{"--experimental-strip-types", runner, "--url", origin, "--token", "performance-token", "--json"}
	if os.Getenv("FORGE_PERF") == "1" {
		args = append(args, "--perf")
	}
	command := exec.Command("node", args...)
	command.Env = append(os.Environ(), "GOMAXPROCS=2")
	output, err := command.CombinedOutput()
	if destination := os.Getenv("FORGE_CONFORMANCE_OUTPUT"); destination != "" {
		if writeErr := os.WriteFile(destination, output, 0644); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	if err != nil {
		t.Fatalf("runner: %v\n%s", err, output)
	}
	start := bytes.IndexByte(output, '{')
	var result struct {
		Counts struct {
			Registered int `json:"registered"`
			Passed     int `json:"passed"`
		} `json:"counts"`
	}
	if start < 0 || json.Unmarshal(output[start:], &result) != nil || result.Counts.Passed != result.Counts.Registered || result.Counts.Registered == 0 {
		t.Fatalf("conformance failure: %s", output)
	}
	t.Log(strings.TrimSpace(string(output)))
}
