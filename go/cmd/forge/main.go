package main

import (
	"bytes"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type record map[string]any
type forge struct {
	guard                           sync.RWMutex
	repos                           map[string]*repository
	owners                          map[string]bool
	ids                             atomic.Int64
	requests                        atomic.Uint64
	auth, login, timestamp, session string
	epoch                           int64
	viewerJSON                      json.RawMessage
}
type repository struct {
	guard                            sync.RWMutex
	id                               int64
	owner, name, branch, description string
	urlBase, issueBase, htmlBase     string
	urlSafe                          bool
	private, hasIssues               bool
	objects                          objects
	refs                             map[string]string
	issues                           map[int]*issue
	issueOrder                       []*issue
	issueLists                       sync.Map
	openIssues                       int
	cached                           []byte
	repoPrefix, repoSuffix           []byte
	pulls                            map[int]*pull
	labels                           map[string]record
	nextNumber                       int
	ad                               map[string][]byte
	packCache                        sync.Map
}

func newForge(token, seed, now, login string) *forge {
	date, err := time.Parse(time.RFC3339Nano, now)
	if err != nil {
		date = time.Unix(1735689600, 0)
	}
	if login == "" {
		login = "adaiversen"
	}
	sum := sha1.Sum([]byte("forge/mcp/" + seed + "/" + token))
	f := &forge{repos: map[string]*repository{}, owners: map[string]bool{}, auth: "Bearer " + token, login: login, epoch: date.Unix(), timestamp: date.UTC().Format(time.RFC3339Nano), session: hex.EncodeToString(sum[:12])}
	f.ids.Store(1000)
	f.viewerJSON, _ = json.Marshal(user(login))
	return f
}

var jsonContentType = []string{"application/json"}

func jsonReply(out http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		status = 500
		data = []byte(`{"message":"Encoding error"}`)
	}
	jsonBytes(out, status, data)
}
func jsonBytes(out http.ResponseWriter, status int, data []byte) {
	out.Header()["Content-Type"] = jsonContentType
	out.Header()["Content-Length"] = []string{strconv.Itoa(len(data))}
	out.WriteHeader(status)
	out.Write(data)
}
func problem(out http.ResponseWriter, status int, message string) {
	jsonReply(out, status, record{"message": message})
}
func missing(out http.ResponseWriter)     { problem(out, 404, "Not Found") }
func text(value any) string               { result, _ := value.(string); return result }
func str(value record, key string) string { return text(value[key]) }
func integer(value any) int {
	switch n := value.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		if n == float64(int(n)) {
			return int(n)
		}
	case string:
		i, _ := strconv.Atoi(n)
		return i
	case json.Number:
		i, _ := strconv.Atoi(string(n))
		return i
	}
	return 0
}
func boolean(value record, key string, fallback bool) bool {
	if result, ok := value[key].(bool); ok {
		return result
	}
	return fallback
}
func (f *forge) ServeHTTP(out http.ResponseWriter, req *http.Request) {
	path := strings.Trim(req.URL.Path, "/")
	out.Header().Set("X-GitHub-Request-Id", strconv.FormatUint(f.requests.Add(1), 36))
	if path == "healthz" && req.Method == "GET" {
		jsonBytes(out, 200, []byte(`{"ok":true}`))
		return
	}
	if f.auth == "Bearer " || subtle.ConstantTimeCompare([]byte(req.Header.Get("Authorization")), []byte(f.auth)) != 1 {
		problem(out, 401, "Bad credentials")
		return
	}
	switch path {
	case "mcp":
		f.mcp(out, req)
		return
	case "api/graphql", "graphql":
		f.graphql(out, req)
		return
	}
	if path == "api/v3" {
		path = ""
	} else if rest, ok := strings.CutPrefix(path, "api/v3/"); ok {
		path = rest
	} else if _, tail, ok := strings.Cut(path, "/"); ok {
		if name, _, ok := strings.Cut(tail, "/"); ok && strings.HasSuffix(name, ".git") {
			f.gitHTTP(out, req, strings.Split(path, "/"))
			return
		}
	}
	f.rest(out, req, path)
}
func key(owner, name string) string { return strings.ToLower(owner + "/" + name) }
func (f *forge) repo(owner, name string) *repository {
	f.guard.RLock()
	repo := f.repos[key(owner, name)]
	f.guard.RUnlock()
	return repo
}
func (r *repository) head() string { return "refs/heads/" + r.branch }
func (r *repository) resolve(ref string) string {
	if ref == "" || ref == "HEAD" {
		return r.refs[r.head()]
	}
	if oid := r.refs[ref]; oid != "" {
		return oid
	}
	if oid := r.refs["refs/heads/"+ref]; oid != "" {
		return oid
	}
	if oid := r.refs["refs/tags/"+ref]; oid != "" {
		return oid
	}
	if r.objects[ref] != nil {
		return ref
	}
	if len(ref) >= 4 && len(ref) < 40 {
		var oid string
		for candidate := range r.objects {
			if strings.HasPrefix(candidate, ref) {
				if oid != "" {
					return ""
				}
				oid = candidate
			}
		}
		return oid
	}
	return ""
}
func (r *repository) changed() { r.ad = nil; r.packCache.Clear() }
func (f *forge) commit(r *repository, tree string, parents []string, message string) string {
	var data strings.Builder
	data.WriteString("tree " + tree + "\n")
	for _, parent := range parents {
		if parent != "" {
			data.WriteString("parent " + parent + "\n")
		}
	}
	identity := f.login + " <" + f.login + "@users.noreply.local> " + strconv.FormatInt(f.epoch, 10) + " +0000\n"
	data.WriteString("author " + identity + "committer " + identity + "\n" + strings.TrimRight(message, "\n") + "\n")
	return r.objects.put(1, []byte(data.String()))
}
func main() {
	runtime.GOMAXPROCS(2)
	base := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if base == "" {
		log.Fatal("MAKEFUN_ENV_URL is required")
	}
	f := newForge(os.Getenv("MAKEFUN_SHOP_TOKEN"), os.Getenv("MAKEFUN_TRIAL_SEED"), os.Getenv("MAKEFUN_TRIAL_NOW"), os.Getenv("MAKEFUN_FORGE_LOGIN"))
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Handler: f, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10}
	go server.Serve(listener)
	client := &http.Client{Timeout: 65 * time.Second, Transport: &http.Transport{MaxIdleConns: 2, IdleConnTimeout: 110 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	port := listener.Addr().(*net.TCPAddr).Port
	call := func(method, url string, body []byte) (bool, error) {
		request, err := http.NewRequest(method, url, bytes.NewReader(body))
		if err != nil {
			return false, err
		}
		request.Header.Set("Authorization", f.auth)
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		response, err := client.Do(request)
		if err != nil {
			return false, err
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return false, fmt.Errorf("%s returned %d", method, response.StatusCode)
		}
		if method == "POST" {
			io.Copy(io.Discard, response.Body)
			return false, nil
		}
		var reply struct{ Done bool }
		err = json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(&reply)
		return reply.Done, err
	}
	if _, err := call("POST", base+"/register", fmt.Appendf(nil, `{"port":%d}`, port)); err != nil {
		log.Fatal(err)
	}
	done := os.Getenv("MAKEFUN_DONE_URL")
	if done == "" {
		done = base + "/done"
	}
	for {
		if finished, err := call("GET", done, nil); err == nil && finished {
			server.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
