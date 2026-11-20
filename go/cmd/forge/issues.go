package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

type issue struct {
	id                 int64
	number             int
	title, body, state string
	labels             []record
	comments           []record
	cached             []byte
	cachedState        string
}

func (repo *repository) issueList(state string, ascending bool) []*issue {
	if state == "" {
		state = "open"
	}
	list := make([]*issue, 0, len(repo.issues))
	for index := 0; index < len(repo.issueOrder); index++ {
		position := index
		if !ascending {
			position = len(repo.issueOrder) - 1 - index
		}
		if item := repo.issueOrder[position]; state == "all" || item.state == state {
			list = append(list, item)
		}
	}
	return list
}
func (f *forge) issueView(repo *repository, item *issue) record {
	number := strconv.Itoa(item.number)
	path := repoURL(repo) + "/issues/" + number
	var closed any
	if item.state == "closed" {
		closed = f.timestamp
	}
	return record{"id": item.id, "node_id": node("Issue", item.id), "number": item.number, "title": item.title, "body": item.body, "state": item.state, "user": user(f.login), "labels": item.labels, "assignees": []record{}, "assignee": nil, "milestone": nil, "locked": false, "comments": len(item.comments), "created_at": f.timestamp, "updated_at": f.timestamp, "closed_at": closed, "url": path, "repository_url": repoURL(repo), "comments_url": path + "/comments", "labels_url": path + "/labels{/name}", "html_url": "/" + repo.owner + "/" + repo.name + "/issues/" + number, "author_association": "OWNER"}
}
func issueLabels(repo *repository, value any) ([]record, *fault) {
	result := []record{}
	if value == nil {
		return result, nil
	}
	list, ok := value.([]any)
	if !ok {
		return nil, fail(422, "Labels must be an array")
	}
	for _, part := range list {
		name := text(part)
		if label, ok := part.(map[string]any); ok {
			name = text(label["name"])
		}
		found := repo.labels[strings.ToLower(name)]
		if found == nil {
			return nil, fail(422, "Label does not exist: "+name)
		}
		result = append(result, found)
	}
	return result, nil
}
func (f *forge) newIssue(repo *repository, input record) (*issue, *fault) {
	return f.newIssueValues(repo, str(input, "title"), str(input, "body"), input["labels"])
}
func (f *forge) newIssueValues(repo *repository, title, body string, labelInput any) (*issue, *fault) {
	if strings.TrimSpace(title) == "" {
		return nil, fail(422, "Title is required")
	}
	if !repo.hasIssues {
		return nil, fail(410, "Issues are disabled")
	}
	labels, err := issueLabels(repo, labelInput)
	if err != nil {
		return nil, err
	}
	repo.nextNumber++
	item := &issue{id: f.ids.Add(1), number: repo.nextNumber, title: title, body: body, state: "open", labels: labels, comments: []record{}}
	repo.issues[item.number] = item
	repo.issueOrder = append(repo.issueOrder, item)
	f.cacheIssue(repo, item)
	return item, nil
}
func (f *forge) cacheIssue(repo *repository, item *issue) {
	if item.cachedState != item.state {
		if item.cachedState == "open" {
			repo.openIssues--
		}
		if item.state == "open" {
			repo.openIssues++
		}
		item.cachedState = item.state
		repo.cacheOpenIssues()
	}
	repo.issueLists.Clear()
	data := make([]byte, 0, 1024+len(item.title)+len(item.body))
	data = strconv.AppendInt(append(data, `{"id":`...), item.id, 10)
	data = append(data, `,"node_id":"Issue_`...)
	data = strconv.AppendInt(data, item.id, 10)
	data = append(data, '"')
	data = strconv.AppendInt(append(data, `,"number":`...), int64(item.number), 10)
	data = appendJSONString(append(data, `,"title":`...), item.title)
	data = appendJSONString(append(data, `,"body":`...), item.body)
	data = appendJSONString(append(data, `,"state":`...), item.state)
	data = append(append(data, `,"user":`...), f.viewerJSON...)
	data = append(data, `,"labels":`...)
	if len(item.labels) == 0 {
		data = append(data, "[]"...)
	} else {
		labels, _ := json.Marshal(item.labels)
		data = append(data, labels...)
	}
	data = append(data, `,"assignees":[],"assignee":null,"milestone":null,"locked":false,"comments":`...)
	data = strconv.AppendInt(data, int64(len(item.comments)), 10)
	data = appendJSONString(append(data, `,"created_at":`...), f.timestamp)
	data = appendJSONString(append(data, `,"updated_at":`...), f.timestamp)
	data = append(data, `,"closed_at":`...)
	if item.state == "closed" {
		data = appendJSONString(data, f.timestamp)
	} else {
		data = append(data, "null"...)
	}
	if repo.urlSafe {
		var numberBuf [20]byte
		number := strconv.AppendInt(numberBuf[:0], int64(item.number), 10)
		data = append(data, `,"url":"`...)
		data = append(data, repo.issueBase...)
		data = append(data, number...)
		data = append(data, `","repository_url":"`...)
		data = append(data, repo.urlBase...)
		data = append(data, `","comments_url":"`...)
		data = append(data, repo.issueBase...)
		data = append(data, number...)
		data = append(data, `/comments","labels_url":"`...)
		data = append(data, repo.issueBase...)
		data = append(data, number...)
		data = append(data, `/labels{/name}","html_url":"`...)
		data = append(data, repo.htmlBase...)
		data = append(data, number...)
		data = append(data, '"')
	} else {
		number := strconv.Itoa(item.number)
		repositoryURL := repo.urlBase
		issueURL := repo.issueBase + number
		data = appendJSONString(append(data, `,"url":`...), issueURL)
		data = appendJSONString(append(data, `,"repository_url":`...), repositoryURL)
		data = appendJSONString(append(data, `,"comments_url":`...), issueURL+"/comments")
		data = appendJSONString(append(data, `,"labels_url":`...), issueURL+"/labels{/name}")
		data = appendJSONString(append(data, `,"html_url":`...), repo.htmlBase+number)
	}
	item.cached = append(data, `,"author_association":"OWNER"}`...)
}

func appendJSONString(data []byte, value string) []byte {
	for index := 0; index < len(value); index++ {
		char := value[index]
		if char < 32 || char == '"' || char == '\\' || char == '<' || char == '>' || char == '&' || char >= utf8.RuneSelf {
			encoded, _ := json.Marshal(value)
			return append(data, encoded...)
		}
	}
	return append(append(append(data, '"'), value...), '"')
}

type issueListCache struct {
	once sync.Once
	body []byte
	next string
}

type issueInput struct {
	Title  any `json:"title"`
	Body   any `json:"body"`
	Labels any `json:"labels"`
}

var issueInputs = sync.Pool{New: func() any { return new(issueInput) }}

// tryDecodeIssue is a fast path that fills input exactly like
// encoding/json.Unmarshal would for a plain JSON object with only the keys
// title/body/labels and unescaped, valid-UTF-8 string values. On anything
// unusual (escapes, other keys, other value types, invalid UTF-8, trailing
// garbage) it reports false and the caller falls back to encoding/json,
// so accepted input semantics never change. Values are only written to
// input on success.
func tryDecodeIssue(data []byte, input *issueInput) bool {
	var title, body string
	var labels []any
	hasTitle, hasBody, hasLabels := false, false, false
	pos := 0
	skip := func() {
		for pos < len(data) && (data[pos] == ' ' || data[pos] == '\t' || data[pos] == '\n' || data[pos] == '\r') {
			pos++
		}
	}
	quoted := func() (string, bool) {
		if pos >= len(data) || data[pos] != '"' {
			return "", false
		}
		pos++
		start := pos
		for pos < len(data) && data[pos] != '"' {
			if data[pos] == '\\' || data[pos] < 32 {
				return "", false
			}
			pos++
		}
		if pos >= len(data) {
			return "", false
		}
		value := data[start:pos]
		pos++
		if !utf8.Valid(value) {
			return "", false
		}
		return string(value), true
	}
	skip()
	if pos >= len(data) || data[pos] != '{' {
		return false
	}
	pos++
	skip()
	if pos < len(data) && data[pos] == '}' {
		pos++
		skip()
		if pos != len(data) {
			return false
		}
		if hasTitle {
			input.Title = title
		}
		if hasBody {
			input.Body = body
		}
		if hasLabels {
			input.Labels = labels
		}
		return true
	}
	for {
		key, ok := quoted()
		if !ok {
			return false
		}
		skip()
		if pos >= len(data) || data[pos] != ':' {
			return false
		}
		pos++
		skip()
		switch key {
		case "title", "body":
			value, ok := quoted()
			if !ok {
				return false
			}
			if key == "title" {
				title = value
				hasTitle = true
			} else {
				body = value
				hasBody = true
			}
		case "labels":
			if pos >= len(data) || data[pos] != '[' {
				return false
			}
			pos++
			skip()
			list := []any{}
			if pos < len(data) && data[pos] != ']' {
				for {
					value, ok := quoted()
					if !ok {
						return false
					}
					list = append(list, value)
					skip()
					if pos < len(data) && data[pos] == ',' {
						pos++
						skip()
						continue
					}
					break
				}
			}
			if pos >= len(data) || data[pos] != ']' {
				return false
			}
			pos++
			labels = list
			hasLabels = true
		default:
			return false
		}
		skip()
		if pos < len(data) && data[pos] == ',' {
			pos++
			skip()
			continue
		}
		break
	}
	if pos >= len(data) || data[pos] != '}' {
		return false
	}
	pos++
	skip()
	if pos != len(data) {
		return false
	}
	if hasTitle {
		input.Title = title
	}
	if hasBody {
		input.Body = body
	}
	if hasLabels {
		input.Labels = labels
	}
	return true
}

// readBody reads up to 16MB+1 from req.Body without per-call wrapper
// allocations (no io.LimitReader struct, no ReadAll growth copies).
// Returns ok=false on read error. The caller keeps the len > 16MB check.
func readBody(req *http.Request) ([]byte, bool) {
	const maxBody = int64(16<<20) + 1
	size := 512
	if req.ContentLength > 0 && req.ContentLength < int64(size) {
		size = int(req.ContentLength)
	}
	data := make([]byte, 0, size)
	for int64(len(data)) < maxBody {
		if len(data) == cap(data) {
			grown := append(data, 0)
			data = grown[:len(grown)-1]
		}
		end := cap(data)
		if int64(end) > maxBody {
			end = int(maxBody)
		}
		read, err := req.Body.Read(data[len(data):end])
		data = data[:len(data)+read]
		if err != nil {
			return data, err == io.EOF
		}
	}
	return data, true
}

func (f *forge) createIssueHTTP(out http.ResponseWriter, req *http.Request, owner, name string) {
	input := issueInputs.Get().(*issueInput)
	defer func() {
		*input = issueInput{}
		issueInputs.Put(input)
	}()
	data, ok := readBody(req)
	if !ok || len(data) > 16<<20 {
		problem(out, 400, "Problems parsing JSON")
		return
	}
	if len(data) > 0 && !tryDecodeIssue(data, input) {
		if json.Unmarshal(data, input) != nil || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
			problem(out, 400, "Problems parsing JSON")
			return
		}
	}
	repo := f.repo(owner, name)
	if repo == nil {
		missing(out)
		return
	}
	repo.guard.Lock()
	item, failure := f.newIssueValues(repo, text(input.Title), text(input.Body), input.Labels)
	var body []byte
	if failure == nil {
		body = item.cached
	}
	repo.guard.Unlock()
	if failure != nil {
		problem(out, failure.status, failure.message)
	} else {
		jsonBytes(out, 201, body)
	}
}

func (repo *repository) cachedIssueList(req *http.Request) *issueListCache {
	shape := req.URL.RawQuery
	value, ok := repo.issueLists.Load(shape)
	if !ok {
		value, _ = repo.issueLists.LoadOrStore(shape, &issueListCache{})
	}
	cached := value.(*issueListCache)
	cached.once.Do(func() {
		query := req.URL.Query()
		size := integer(query.Get("per_page"))
		if size <= 0 {
			size = 30
		}
		size = min(size, 100)
		page := max(1, integer(query.Get("page")))
		list := repo.issueList(query.Get("state"), query.Get("direction") == "asc")
		start, end := 0, 0
		if page <= len(list)/size+1 {
			start = (page - 1) * size
			end = min(start+size, len(list))
		}
		if end < len(list) && end > start {
			query.Set("page", strconv.Itoa(page+1))
			cached.next = query.Encode()
		}
		capacity := 2
		for _, item := range list[start:end] {
			capacity += len(item.cached) + 1
		}
		body := make([]byte, 1, capacity)
		body[0] = '['
		for index, item := range list[start:end] {
			if index > 0 {
				body = append(body, ',')
			}
			body = append(body, item.cached...)
		}
		cached.body = append(body, ']')
	})
	return cached
}

type issueJSON struct {
	ID                int64           `json:"id"`
	NodeID            string          `json:"node_id"`
	Number            int             `json:"number"`
	Title             string          `json:"title"`
	Body              string          `json:"body"`
	State             string          `json:"state"`
	User              json.RawMessage `json:"user"`
	Labels            []record        `json:"labels"`
	Assignees         []record        `json:"assignees"`
	Assignee          *record         `json:"assignee"`
	Milestone         *record         `json:"milestone"`
	Locked            bool            `json:"locked"`
	Comments          int             `json:"comments"`
	CreatedAt         string          `json:"created_at"`
	UpdatedAt         string          `json:"updated_at"`
	ClosedAt          *string         `json:"closed_at"`
	URL               string          `json:"url"`
	RepositoryURL     string          `json:"repository_url"`
	CommentsURL       string          `json:"comments_url"`
	LabelsURL         string          `json:"labels_url"`
	HTMLURL           string          `json:"html_url"`
	AuthorAssociation string          `json:"author_association"`
}

func (f *forge) comment(repo *repository, item *issue, body string) (record, *fault) {
	if strings.TrimSpace(body) == "" {
		return nil, fail(422, "Body is required")
	}
	id := f.ids.Add(1)
	issueURL := repoURL(repo) + "/issues/" + strconv.Itoa(item.number)
	identifier := strconv.FormatInt(id, 10)
	value := record{"id": id, "node_id": node("IssueComment", id), "body": body, "user": user(f.login), "created_at": f.timestamp, "updated_at": f.timestamp, "url": repoURL(repo) + "/issues/comments/" + identifier, "issue_url": issueURL, "html_url": "/" + repo.owner + "/" + repo.name + "/issues/" + strconv.Itoa(item.number) + "#issuecomment-" + identifier, "author_association": "OWNER"}
	item.comments = append(item.comments, value)
	f.cacheIssue(repo, item)
	return value, nil
}
func (f *forge) restIssues(out http.ResponseWriter, req *http.Request, repo *repository, parts []string, input record) {
	switch len(parts) {
	case 0:
		if req.Method == "POST" {
			item, err := f.newIssue(repo, input)
			if err != nil {
				respond(out, 201, nil, err)
			} else {
				jsonBytes(out, 201, item.cached)
			}
			return
		}
		if req.Method == "GET" {
			cached := repo.cachedIssueList(req)
			if cached.next != "" {
				out.Header().Set("Link", "<http://"+req.Host+req.URL.EscapedPath()+"?"+cached.next+">; rel=\"next\"")
			}
			jsonBytes(out, 200, cached.body)
			return
		}
	case 1, 2:
		item := repo.issues[integer(parts[0])]
		if item == nil {
			break
		}
		if len(parts) == 2 {
			if parts[1] != "comments" {
				break
			}
			if req.Method == "GET" {
				jsonReply(out, 200, paginate(out, req, item.comments, 30))
				return
			}
			if req.Method == "POST" {
				comment, err := f.comment(repo, item, str(input, "body"))
				respond(out, 201, comment, err)
				return
			}
			break
		}
		if req.Method == "GET" {
			jsonBytes(out, 200, item.cached)
			return
		}
		if req.Method == "PATCH" {
			labels := item.labels
			if values, ok := input["labels"]; ok {
				var err *fault
				labels, err = issueLabels(repo, values)
				if err != nil {
					respond(out, 200, nil, err)
					return
				}
			}
			if title, ok := input["title"].(string); ok {
				if strings.TrimSpace(title) == "" {
					problem(out, 422, "Title is required")
					return
				}
				item.title = title
			}
			if body, ok := input["body"].(string); ok {
				item.body = body
			}
			if state := str(input, "state"); state == "open" || state == "closed" {
				item.state = state
			}
			item.labels = labels
			f.cacheIssue(repo, item)
			jsonBytes(out, 200, item.cached)
			return
		}
	}
	missing(out)
}
func (f *forge) restLabels(out http.ResponseWriter, req *http.Request, repo *repository, name string, input record) {
	if req.Method == "POST" && name == "" {
		name = str(input, "name")
		key := strings.ToLower(name)
		if name == "" || repo.labels[key] != nil {
			problem(out, 422, "Invalid or duplicate label")
			return
		}
		id := f.ids.Add(1)
		value := record{"id": id, "node_id": node("Label", id), "name": name, "color": str(input, "color"), "description": str(input, "description"), "default": false, "url": repoURL(repo) + "/labels/" + escapePath(name)}
		repo.labels[key] = value
		jsonReply(out, 201, value)
		return
	}
	if req.Method == "GET" {
		if name == "" {
			list := []record{}
			for _, key := range slices.Sorted(mapKeys(repo.labels)) {
				list = append(list, repo.labels[key])
			}
			jsonReply(out, 200, paginate(out, req, list, 30))
			return
		}
		if label := repo.labels[strings.ToLower(name)]; label != nil {
			jsonReply(out, 200, label)
			return
		}
	}
	missing(out)
}
