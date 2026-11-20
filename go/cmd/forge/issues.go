package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

type issue struct {
	id                 int64
	number             int
	title, body, state string
	labels             []record
	comments           []record
	cached             []byte
}

func (repo *repository) issueList(state string, ascending bool) []*issue {
	if state == "" {
		state = "open"
	}
	list := make([]*issue, 0, len(repo.issues))
	for index := 1; index <= repo.nextNumber; index++ {
		number := index
		if !ascending {
			number = repo.nextNumber + 1 - index
		}
		if item := repo.issues[number]; item != nil && (state == "all" || item.state == state) {
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
	title := str(input, "title")
	if strings.TrimSpace(title) == "" {
		return nil, fail(422, "Title is required")
	}
	if !repo.hasIssues {
		return nil, fail(410, "Issues are disabled")
	}
	labels, err := issueLabels(repo, input["labels"])
	if err != nil {
		return nil, err
	}
	repo.nextNumber++
	item := &issue{id: f.ids.Add(1), number: repo.nextNumber, title: title, body: str(input, "body"), state: "open", labels: labels, comments: []record{}}
	repo.issues[item.number] = item
	f.cacheIssue(repo, item)
	return item, nil
}
func (f *forge) cacheIssue(repo *repository, item *issue) {
	number := strconv.Itoa(item.number)
	repositoryURL := repoURL(repo)
	issueURL := repositoryURL + "/issues/" + number
	var closedAt *string
	if item.state == "closed" {
		closedAt = &f.timestamp
	}
	item.cached, _ = json.Marshal(issueJSON{
		ID: item.id, NodeID: node("Issue", item.id), Number: item.number, Title: item.title,
		Body: item.body, State: item.state, User: f.viewerJSON, Labels: item.labels,
		Assignees: []record{}, Comments: len(item.comments), CreatedAt: f.timestamp,
		UpdatedAt: f.timestamp, ClosedAt: closedAt, URL: issueURL, RepositoryURL: repositoryURL,
		CommentsURL: issueURL + "/comments", LabelsURL: issueURL + "/labels{/name}",
		HTMLURL: "/" + repo.owner + "/" + repo.name + "/issues/" + number, AuthorAssociation: "OWNER",
	})
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
			query := req.URL.Query()
			list := paginate(out, req, repo.issueList(query.Get("state"), query.Get("direction") == "asc"), 30)
			body := []byte{'['}
			for index, item := range list {
				if index > 0 {
					body = append(body, ',')
				}
				body = append(body, item.cached...)
			}
			jsonBytes(out, 200, append(body, ']'))
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
