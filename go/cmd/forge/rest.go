package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

type fault struct {
	status  int
	message string
}

func fail(status int, message string) *fault { return &fault{status, message} }
func respond(out http.ResponseWriter, status int, value any, err *fault) {
	if err != nil {
		problem(out, err.status, err.message)
	} else {
		jsonReply(out, status, value)
	}
}
func decode(req *http.Request) (record, bool) {
	data, err := io.ReadAll(io.LimitReader(req.Body, (16<<20)+1))
	if err != nil || len(data) > 16<<20 {
		return nil, false
	}
	if len(data) == 0 {
		return record{}, true
	}
	var input record
	err = json.Unmarshal(data, &input)
	return input, err == nil && input != nil
}
func repoURL(repo *repository) string   { return "/api/v3/repos/" + repo.owner + "/" + repo.name }
func node(kind string, id int64) string { return kind + "_" + strconv.FormatInt(id, 10) }
func user(login string) record {
	return record{"login": login, "id": 1000, "node_id": "User_1000", "type": "User", "site_admin": false, "url": "/api/v3/users/" + login, "html_url": "/" + login, "avatar_url": ""}
}
func (f *forge) repoView(repo *repository) record {
	path := repoURL(repo)
	full := repo.owner + "/" + repo.name
	owner := user(repo.owner)
	if repo.owner != f.login {
		owner["type"] = "Organization"
	}
	return record{"id": repo.id, "node_id": node("Repository", repo.id), "name": repo.name, "full_name": full, "owner": owner, "private": repo.private, "description": repo.description, "default_branch": repo.branch, "has_issues": repo.hasIssues, "has_projects": true, "has_wiki": true, "fork": false, "archived": false, "disabled": false, "url": path, "html_url": "/" + full, "clone_url": "/" + full + ".git", "git_url": "/" + full + ".git", "ssh_url": "git@localhost:" + full + ".git", "created_at": f.timestamp, "updated_at": f.timestamp, "pushed_at": f.timestamp, "permissions": record{"admin": true, "push": true, "pull": true}, "issues_url": path + "/issues{/number}", "pulls_url": path + "/pulls{/number}", "contents_url": path + "/contents/{+path}", "open_issues_count": repo.openIssues, "visibility": map[bool]string{true: "private", false: "public"}[repo.private]}
}
func (f *forge) cacheRepo(repo *repository) {
	repo.cached, _ = json.Marshal(f.repoView(repo))
	marker := []byte(`"open_issues_count":`)
	start := bytes.Index(repo.cached, marker) + len(marker)
	end := start
	for end < len(repo.cached) && repo.cached[end] >= '0' && repo.cached[end] <= '9' {
		end++
	}
	repo.repoPrefix, repo.repoSuffix = repo.cached[:start], repo.cached[end:]
}
func (repo *repository) cacheOpenIssues() {
	if repo.repoPrefix == nil {
		return
	}
	data := make([]byte, 0, len(repo.repoPrefix)+len(repo.repoSuffix)+20)
	data = append(data, repo.repoPrefix...)
	data = strconv.AppendInt(data, int64(repo.openIssues), 10)
	repo.cached = append(data, repo.repoSuffix...)
}
func (f *forge) createRepo(owner string, input record) (*repository, *fault) {
	name := str(input, "name")
	if name == "" || len(name) > 100 || name == "." || name == ".." || strings.ContainsAny(name, " /\\\x00") {
		return nil, fail(422, "Invalid repository name")
	}
	f.guard.Lock()
	defer f.guard.Unlock()
	lookup := key(owner, name)
	if f.repos[lookup] != nil {
		return nil, fail(422, "Repository already exists")
	}
	repo := &repository{id: f.ids.Add(1), owner: owner, name: name, branch: "main", description: str(input, "description"), private: boolean(input, "private", false), hasIssues: boolean(input, "has_issues", true), objects: objects{}, refs: map[string]string{}, issues: map[int]*issue{}, pulls: map[int]*pull{}, labels: map[string]record{}}
	if boolean(input, "auto_init", false) {
		blob := repo.objects.put(3, []byte("# "+name+"\n"))
		tree := repo.objects.makeTree(map[string]treeEntry{"README.md": {"100644", "README.md", blob}})
		repo.refs[repo.head()] = f.commit(repo, tree, nil, "Initial commit")
	}
	f.cacheRepo(repo)
	f.repos[lookup] = repo
	if owner != f.login {
		f.owners[owner] = true
	}
	return repo, nil
}
func (f *forge) reposList(owner string) []*repository {
	f.guard.RLock()
	list := make([]*repository, 0, len(f.repos))
	for _, repo := range f.repos {
		if owner == "" || strings.EqualFold(repo.owner, owner) {
			list = append(list, repo)
		}
	}
	f.guard.RUnlock()
	slices.SortFunc(list, func(a, b *repository) int {
		if a.id < b.id {
			return -1
		}
		return 1
	})
	return list
}
func paginate[T any](out http.ResponseWriter, req *http.Request, list []T, defaultSize int) []T {
	query := req.URL.Query()
	size := integer(query.Get("per_page"))
	if size <= 0 {
		size = defaultSize
	}
	size = min(size, 100)
	page := max(1, integer(query.Get("page")))
	if page > len(list)/size+1 {
		return list[:0]
	}
	start := (page - 1) * size
	end := min(start+size, len(list))
	if end < len(list) {
		query.Set("page", strconv.Itoa(page+1))
		out.Header().Set("Link", "<http://"+req.Host+req.URL.EscapedPath()+"?"+query.Encode()+">; rel=\"next\"")
	}
	return list[start:end]
}
func (f *forge) orgs() []record {
	f.guard.RLock()
	defer f.guard.RUnlock()
	list := []record{}
	for _, owner := range slices.Sorted(mapKeys(f.owners)) {
		list = append(list, organization(owner))
	}
	return list
}
func organization(owner string) record {
	return record{"login": owner, "id": 2000, "node_id": "Organization_2000", "url": "/api/v3/orgs/" + owner, "repos_url": "/api/v3/orgs/" + owner + "/repos", "type": "Organization"}
}
func (f *forge) rest(out http.ResponseWriter, req *http.Request, path string) {
	method := req.Method
	if method == "GET" && f.cachedGet(out, req, path) {
		return
	}
	parts := strings.Split(path, "/")
	if method == "POST" && len(parts) == 4 && parts[0] == "repos" && parts[3] == "issues" {
		f.createIssueHTTP(out, req, parts[1], parts[2])
		return
	}
	var input record
	if method == "POST" || method == "PATCH" || method == "PUT" || method == "DELETE" {
		var ok bool
		input, ok = decode(req)
		if !ok {
			problem(out, 400, "Problems parsing JSON")
			return
		}
	}
	if method == "GET" {
		switch path {
		case "":
			out.Header().Set("X-OAuth-Scopes", "repo, read:org")
			jsonReply(out, 200, record{"current_user_url": "/api/v3/user", "repository_url": "/api/v3/repos/{owner}/{repo}", "rate_limit_url": "/api/v3/rate_limit"})
			return
		case "meta":
			jsonReply(out, 200, record{"installed_version": "2.9.200"})
			return
		case "user":
			jsonReply(out, 200, user(f.login))
			return
		case "notifications":
			jsonBytes(out, 200, []byte("[]"))
			return
		case "search/issues":
			f.searchIssues(out, req)
			return
		}
	}
	if path == "user/repos" || path == "repositories" || (len(parts) == 3 && (parts[0] == "orgs" || parts[0] == "users") && parts[2] == "repos") {
		owner := f.login
		if path == "repositories" {
			owner = ""
		}
		if len(parts) == 3 {
			owner = parts[1]
		}
		if method == "POST" && (path == "user/repos" || parts[0] == "orgs") {
			repo, err := f.createRepo(owner, input)
			if err != nil {
				respond(out, 201, nil, err)
				return
			}
			repo.guard.RLock()
			view := repo.cached
			repo.guard.RUnlock()
			jsonBytes(out, 201, view)
			return
		}
		if method == "GET" {
			list := f.reposList(owner)
			if path == "repositories" {
				since := int64(integer(req.URL.Query().Get("since")))
				for len(list) > 0 && list[0].id <= since {
					list = list[1:]
				}
			}
			views := make([]byte, 1, 1024)
			views[0] = '['
			for _, repo := range paginate(out, req, list, 30) {
				if len(views) > 1 {
					views = append(views, ',')
				}
				repo.guard.RLock()
				views = append(views, repo.cached...)
				repo.guard.RUnlock()
			}
			jsonBytes(out, 200, append(views, ']'))
			return
		}
	}
	if method == "GET" && len(parts) >= 2 {
		if len(parts) == 2 && parts[0] == "users" {
			if !strings.EqualFold(parts[1], f.login) {
				missing(out)
				return
			}
			jsonReply(out, 200, user(f.login))
			return
		}
		if len(parts) == 3 && parts[0] == "users" && parts[2] == "orgs" {
			if !strings.EqualFold(parts[1], f.login) {
				missing(out)
				return
			}
			jsonReply(out, 200, f.orgs())
			return
		}
		if len(parts) == 2 && parts[0] == "orgs" {
			f.guard.RLock()
			exists := f.owners[parts[1]]
			f.guard.RUnlock()
			if !exists {
				missing(out)
				return
			}
			jsonReply(out, 200, organization(parts[1]))
			return
		}
	}
	if len(parts) < 3 || parts[0] != "repos" {
		missing(out)
		return
	}
	repo := f.repo(parts[1], parts[2])
	if repo == nil {
		missing(out)
		return
	}
	if len(parts) == 3 && method == "DELETE" {
		f.guard.Lock()
		delete(f.repos, key(parts[1], parts[2]))
		f.guard.Unlock()
		out.WriteHeader(204)
		return
	}
	if method == "GET" {
		repo.guard.RLock()
		defer repo.guard.RUnlock()
	} else {
		repo.guard.Lock()
		defer repo.guard.Unlock()
	}
	if len(parts) == 3 {
		if method == "PATCH" {
			if value, ok := input["description"].(string); ok {
				repo.description = value
			}
			repo.private = boolean(input, "private", repo.private)
			repo.hasIssues = boolean(input, "has_issues", repo.hasIssues)
			if branch := str(input, "default_branch"); branch != "" && repo.refs["refs/heads/"+branch] != "" {
				repo.branch = branch
				repo.changed()
			}
			f.cacheRepo(repo)
		} else if method != "GET" {
			missing(out)
			return
		}
		jsonBytes(out, 200, repo.cached)
		return
	}
	resource := parts[3]
	tail := parts[4:]
	item := strings.Join(tail, "/")
	switch resource {
	case "issues":
		f.restIssues(out, req, repo, tail, input)
		return
	case "pulls":
		f.restPulls(out, req, repo, tail, input)
		return
	case "labels":
		f.restLabels(out, req, repo, item, input)
		return
	case "contents":
		f.contents(out, req, repo, item, input)
		return
	case "git":
		f.restGit(out, req, repo, tail, input)
		return
	}
	if method != "GET" {
		missing(out)
		return
	}
	switch resource {
	case "branches":
		if item != "" {
			oid := repo.refs["refs/heads/"+item]
			if oid == "" {
				missing(out)
				return
			}
			jsonReply(out, 200, branchView(repo, item, oid))
			return
		}
		list := []record{}
		for _, name := range refNames(repo.refs) {
			if branch, ok := strings.CutPrefix(name, "refs/heads/"); ok {
				list = append(list, branchView(repo, branch, repo.refs[name]))
			}
		}
		jsonReply(out, 200, paginate(out, req, list, 30))
		return
	case "tags":
		list := []record{}
		for _, name := range refNames(repo.refs) {
			if tag, ok := strings.CutPrefix(name, "refs/tags/"); ok {
				itemView := record{"name": tag, "commit": f.commitPointer(repo, repo.objects.peel(repo.refs[name])), "zipball_url": repoURL(repo) + "/zipball/" + tag, "tarball_url": repoURL(repo) + "/tarball/" + tag}
				if item == tag {
					jsonReply(out, 200, itemView)
					return
				}
				list = append(list, itemView)
			}
		}
		if item != "" {
			missing(out)
			return
		}
		jsonReply(out, 200, paginate(out, req, list, 30))
		return
	case "releases":
		if item == "" {
			jsonBytes(out, 200, []byte("[]"))
			return
		}
	case "commits":
		if item != "" {
			oid := repo.objects.peel(repo.resolve(item))
			if obj := repo.objects[oid]; obj == nil || obj.kind != 1 {
				missing(out)
				return
			}
			jsonReply(out, 200, f.commitView(repo, oid))
			return
		}
		list := []record{}
		stack := []string{repo.resolve(req.URL.Query().Get("sha"))}
		seen := map[string]bool{}
		for len(stack) > 0 {
			last := len(stack) - 1
			oid := stack[last]
			stack = stack[:last]
			obj := repo.objects[oid]
			if obj == nil || obj.kind != 1 || seen[oid] {
				continue
			}
			seen[oid] = true
			list = append(list, f.commitView(repo, oid))
			for index := len(obj.parents) - 1; index >= 0; index-- {
				stack = append(stack, obj.parents[index])
			}
		}
		jsonReply(out, 200, paginate(out, req, list, 30))
		return
	}
	missing(out)
}

func (f *forge) cachedGet(out http.ResponseWriter, req *http.Request, path string) bool {
	path, ok := strings.CutPrefix(path, "repos/")
	if !ok {
		return false
	}
	owner, path, ok := strings.Cut(path, "/")
	if !ok {
		return false
	}
	name, tail, _ := strings.Cut(path, "/")
	if tail != "" && tail != "issues" && (!strings.HasPrefix(tail, "issues/") || strings.Contains(tail[7:], "/")) {
		return false
	}
	repo := f.repo(owner, name)
	if repo == nil {
		missing(out)
		return true
	}
	repo.guard.RLock()
	var body []byte
	var next string
	switch tail {
	case "":
		body = repo.cached
	case "issues":
		cached := repo.cachedIssueList(req)
		body, next = cached.body, cached.next
	default:
		if item := repo.issues[integer(tail[7:])]; item != nil {
			body = item.cached
		}
	}
	repo.guard.RUnlock()
	if body == nil {
		missing(out)
	} else {
		if next != "" {
			out.Header().Set("Link", "<http://"+req.Host+req.URL.EscapedPath()+"?"+next+">; rel=\"next\"")
		}
		jsonBytes(out, 200, body)
	}
	return true
}
func branchView(repo *repository, branch, oid string) record {
	return record{"name": branch, "protected": false, "commit": record{"sha": oid, "url": repoURL(repo) + "/commits/" + oid}, "protection": record{"enabled": false}}
}
func (f *forge) searchIssues(out http.ResponseWriter, req *http.Request) {
	query := req.URL.Query().Get("q")
	state := "all"
	var repos []*repository
	var terms []string
	for term := range strings.FieldsSeq(query) {
		if full, ok := strings.CutPrefix(term, "repo:"); ok {
			owner, name, valid := strings.Cut(full, "/")
			if valid {
				if repo := f.repo(owner, name); repo != nil {
					repos = append(repos, repo)
				}
			}
			if repos == nil {
				repos = []*repository{}
			}
		} else if filter, ok := strings.CutPrefix(term, "is:"); ok {
			if filter == "open" || filter == "closed" {
				state = filter
			}
		} else {
			terms = append(terms, strings.ToLower(term))
		}
	}
	if repos == nil {
		repos = f.reposList("")
	}
	items := []record{}
	for _, repo := range repos {
		repo.guard.RLock()
		for _, issue := range repo.issueList(state, false) {
			hay := strings.ToLower(issue.title + " " + issue.body)
			if !slices.ContainsFunc(terms, func(term string) bool { return !strings.Contains(hay, term) }) {
				items = append(items, f.issueView(repo, issue))
			}
		}
		repo.guard.RUnlock()
	}
	jsonReply(out, 200, record{"total_count": len(items), "incomplete_results": false, "items": paginate(out, req, items, 30)})
}
func (f *forge) commitPointer(repo *repository, oid string) record {
	return record{"sha": oid, "url": repoURL(repo) + "/git/commits/" + oid}
}
func escapePath(path string) string {
	parts := strings.Split(path, "/")
	for index, part := range parts {
		parts[index] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
