package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

type gqlObject struct {
	kind   string
	values record
}
type gqlResolver func(record) (any, *fault)
type gqlExecution struct {
	forge     *forge
	variables record
	fragments map[string]gqlField
	errors    []record
	remaining int
}
type gqlDocument struct {
	Query         string
	Variables     record
	OperationName string
}

func gql(kind string, values record) gqlObject { return gqlObject{kind, values} }
func (f *forge) graphql(out http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		missing(out)
		return
	}
	data, err := io.ReadAll(io.LimitReader(req.Body, 2<<20))
	data = bytes.TrimSpace(data)
	if err != nil || len(data) == 0 {
		problem(out, 400, "Invalid JSON")
		return
	}
	if data[0] == '[' {
		var docs []gqlDocument
		if json.Unmarshal(data, &docs) != nil {
			problem(out, 400, "Invalid JSON")
			return
		}
		results := make([]any, 0, len(docs))
		for _, doc := range docs {
			results = append(results, f.graphOperation(doc))
		}
		jsonReply(out, 200, results)
		return
	}
	var doc gqlDocument
	if json.Unmarshal(data, &doc) != nil || data[0] != '{' {
		problem(out, 400, "Invalid JSON")
		return
	}
	jsonReply(out, 200, f.graphOperation(doc))
}
func (f *forge) graphOperation(doc gqlDocument) any {
	operations, fragments, err := gqlParse(doc.Query)
	if err != nil {
		return record{"errors": []record{{"message": err.Error()}}}
	}
	results := []record{}
	for _, op := range operations {
		if doc.OperationName != "" && op.name != doc.OperationName {
			continue
		}
		exec := gqlExecution{forge: f, variables: op.defaults, fragments: fragments, remaining: 20000}
		for key, value := range doc.Variables {
			exec.variables[key] = value
		}
		result := record{"data": exec.project(exec.root(op.kind), op.fields, 0)}
		if len(exec.errors) > 0 {
			result["errors"] = exec.errors
		}
		results = append(results, result)
	}
	if len(results) == 1 {
		return results[0]
	}
	if len(results) == 0 {
		return record{"errors": []record{{"message": "Unknown operation"}}}
	}
	return results
}
func (exec *gqlExecution) error(message string) {
	if len(exec.errors) < 50 {
		exec.errors = append(exec.errors, record{"message": message})
	}
}
func (exec *gqlExecution) enabled(directives []gqlDirective) bool {
	for _, directive := range directives {
		value, _ := exec.value(directive.value).(bool)
		if directive.name == "skip" && value || directive.name == "include" && !value {
			return false
		}
	}
	return true
}
func (exec *gqlExecution) project(value any, fields []gqlField, depth int) any {
	if depth > 100 || exec.remaining <= 0 {
		exec.error("Selection is too complex")
		return nil
	}
	switch value := value.(type) {
	case gqlObject:
		result := record{}
		for _, field := range fields {
			if exec.remaining--; exec.remaining < 0 {
				exec.error("Selection is too complex")
				break
			}
			if !exec.enabled(field.directives) {
				continue
			}
			if field.name == "" {
				fragment := field
				if field.alias != "" {
					fragment = exec.fragments[field.alias]
				}
				if (fragment.condition == "" || fragment.condition == value.kind || fragment.condition == "Node" || fragment.condition == "Actor") && exec.enabled(fragment.directives) {
					if entries, ok := exec.project(value, fragment.fields, depth+1).(record); ok {
						for key, entry := range entries {
							result[key] = entry
						}
					}
				}
				continue
			}
			if field.name == "__typename" {
				result[field.alias] = value.kind
				continue
			}
			entry, ok := value.values[field.name]
			if !ok {
				exec.error(fmt.Sprintf("Cannot query field %q on type %q.", field.name, value.kind))
				continue
			}
			if resolve, ok := entry.(gqlResolver); ok {
				args := record{}
				for key, arg := range field.args {
					args[key] = exec.value(arg)
				}
				var err *fault
				entry, err = resolve(args)
				if err != nil {
					exec.error(err.message)
				}
			}
			result[field.alias] = exec.project(entry, field.fields, depth+1)
		}
		return result
	case []gqlObject:
		result := make([]any, 0, len(value))
		for _, item := range value {
			if exec.remaining <= 0 {
				break
			}
			result = append(result, exec.project(item, fields, depth+1))
		}
		return result
	default:
		return value
	}
}
func (f *forge) gqlUser(login string) gqlObject {
	return gql("User", record{"id": "User_1000", "databaseId": 1000, "login": login, "name": login, "url": "/" + login, "avatarUrl": ""})
}
func (f *forge) byNode(id string) *repository {
	for _, repo := range f.reposList("") {
		if node("Repository", repo.id) == id {
			return repo
		}
	}
	return nil
}
func (exec *gqlExecution) root(operation string) gqlObject {
	f := exec.forge
	if operation == "mutation" {
		values := record{}
		for _, name := range []string{"createIssue", "closeIssue", "reopenIssue", "createPullRequest", "mergePullRequest", "addComment"} {
			values[name] = gqlResolver(func(args record) (any, *fault) { return f.graphMutate(name, objectRecord(args["input"])) })
		}
		return gql("Mutation", values)
	}
	return gql("Query", record{
		"viewer": f.gqlUser(f.login),
		"repository": gqlResolver(func(args record) (any, *fault) {
			repo := f.repo(str(args, "owner"), str(args, "name"))
			if repo == nil {
				return nil, nil
			}
			return f.graphRepo(repo), nil
		}),
		"node": gqlResolver(func(args record) (any, *fault) {
			id := str(args, "id")
			if repo := f.byNode(id); repo != nil {
				return f.graphRepo(repo), nil
			}
			for _, repo := range f.reposList("") {
				repo.guard.RLock()
				for _, item := range repo.issues {
					if node("Issue", item.id) == id {
						obj := f.graphIssue(item)
						repo.guard.RUnlock()
						return obj, nil
					}
				}
				for _, item := range repo.pulls {
					if node("PullRequest", item.id) == id {
						obj := f.graphPull(repo, item)
						repo.guard.RUnlock()
						return obj, nil
					}
				}
				repo.guard.RUnlock()
			}
			return nil, nil
		}),
		"__type": gqlResolver(func(args record) (any, *fault) { return introspect(str(args, "name")), nil }),
		"__schema": gqlResolver(func(record) (any, *fault) {
			values := []gqlObject{}
			for _, name := range slices.Sorted(mapKeys(graphTypes)) {
				values = append(values, introspect(name).(gqlObject))
			}
			return gql("__Schema", record{"types": values, "queryType": introspect("Query"), "mutationType": introspect("Mutation"), "subscriptionType": nil, "directives": []gqlObject{}}), nil
		}),
	})
}
func connection(kind string, items []gqlObject, args record) gqlObject {
	total := len(items)
	start := min(total, max(0, integer(args["after"])))
	end := total
	if first, exists := args["first"]; exists && first != nil {
		end = min(end, start+min(total, max(0, integer(first))))
	}
	if last, exists := args["last"]; exists && last != nil {
		start = max(start, end-max(0, integer(last)))
	}
	nodes := items[start:end]
	edges := make([]gqlObject, 0, len(nodes))
	for index, item := range nodes {
		edges = append(edges, gql(kind+"Edge", record{"cursor": strconv.Itoa(start + index + 1), "node": item}))
	}
	var startCursor, endCursor any
	if start != end {
		startCursor = strconv.Itoa(start + 1)
		endCursor = strconv.Itoa(end)
	}
	return gql(kind+"Connection", record{"nodes": nodes, "edges": edges, "totalCount": total, "pageInfo": gql("PageInfo", record{"hasNextPage": end < total, "hasPreviousPage": start > 0, "startCursor": startCursor, "endCursor": endCursor})})
}
func matchStates(args record, state string) bool {
	list, ok := args["states"].([]any)
	if !ok {
		if value := str(args, "states"); value != "" {
			return value == state
		}
		return true
	}
	return slices.ContainsFunc(list, func(item any) bool { return text(item) == state })
}
func (f *forge) graphRepo(repo *repository) gqlObject {
	repo.guard.RLock()
	defer repo.guard.RUnlock()
	full := repo.owner + "/" + repo.name
	return gql("Repository", record{"id": node("Repository", repo.id), "databaseId": repo.id, "name": repo.name, "nameWithOwner": full, "owner": f.gqlUser(repo.owner), "defaultBranchRef": gqlRef(repo.branch, repo.refs[repo.head()]), "viewerPermission": "ADMIN", "hasIssuesEnabled": repo.hasIssues, "description": repo.description, "url": "/" + full, "isPrivate": repo.private, "isFork": false, "isArchived": false, "createdAt": f.timestamp, "updatedAt": f.timestamp,
		"issue": gqlResolver(func(args record) (any, *fault) {
			repo.guard.RLock()
			defer repo.guard.RUnlock()
			if item := repo.issues[integer(args["number"])]; item != nil {
				return f.graphIssue(item), nil
			}
			return nil, nil
		}),
		"issues": gqlResolver(func(args record) (any, *fault) {
			repo.guard.RLock()
			defer repo.guard.RUnlock()
			items := []gqlObject{}
			ascending := str(objectRecord(args["orderBy"]), "direction") == "ASC"
			for _, item := range repo.issueList("all", ascending) {
				if matchStates(args, strings.ToUpper(item.state)) {
					items = append(items, f.graphIssue(item))
				}
			}
			return connection("Issue", items, args), nil
		}),
		"pullRequest": gqlResolver(func(args record) (any, *fault) {
			repo.guard.RLock()
			defer repo.guard.RUnlock()
			if item := repo.pulls[integer(args["number"])]; item != nil {
				return f.graphPull(repo, item), nil
			}
			return nil, nil
		}),
		"pullRequests": gqlResolver(func(args record) (any, *fault) {
			repo.guard.RLock()
			defer repo.guard.RUnlock()
			list := repo.pullList("all")
			if str(objectRecord(args["orderBy"]), "direction") == "ASC" {
				slices.Reverse(list)
			}
			items := []gqlObject{}
			for _, item := range list {
				if matchStates(args, pullState(item)) {
					items = append(items, f.graphPull(repo, item))
				}
			}
			return connection("PullRequest", items, args), nil
		}),
	})
}
func gqlRef(name, oid string) gqlObject {
	var target any
	if oid != "" {
		target = gql("Commit", record{"oid": oid, "id": "Commit_" + oid})
	}
	return gql("Ref", record{"name": name, "target": target, "prefix": "refs/heads/"})
}
func (f *forge) graphIssue(item *issue) gqlObject {
	comments := []gqlObject{}
	for _, comment := range item.comments {
		comments = append(comments, f.graphComment(comment))
	}
	labelList := []gqlObject{}
	for _, label := range item.labels {
		labelList = append(labelList, gql("Label", record{"id": label["node_id"], "name": label["name"], "color": label["color"], "description": label["description"]}))
	}
	var closedAt any
	if item.state == "closed" {
		closedAt = f.timestamp
	}
	return gql("Issue", record{"id": node("Issue", item.id), "databaseId": item.id, "number": item.number, "title": item.title, "body": item.body, "bodyText": item.body, "state": strings.ToUpper(item.state), "author": f.gqlUser(f.login), "createdAt": f.timestamp, "updatedAt": f.timestamp, "closedAt": closedAt, "closed": item.state == "closed", "locked": false, "comments": gqlResolver(func(args record) (any, *fault) { return connection("IssueComment", comments, args), nil }), "labels": gqlResolver(func(args record) (any, *fault) { return connection("Label", labelList, args), nil })})
}
func (f *forge) graphComment(comment record) gqlObject {
	return gql("IssueComment", record{"id": comment["node_id"], "databaseId": comment["id"], "author": f.gqlUser(f.login), "body": comment["body"], "createdAt": f.timestamp, "updatedAt": f.timestamp, "url": comment["html_url"]})
}
func pullState(item *pull) string {
	if item.mergedSHA != "" {
		return "MERGED"
	}
	if item.closed {
		return "CLOSED"
	}
	return "OPEN"
}
func (f *forge) graphPull(repo *repository, item *pull) gqlObject {
	mergeable := "UNKNOWN"
	if !item.closed {
		_, _, clean := repo.mergeFiles(item)
		if clean {
			mergeable = "MERGEABLE"
		} else {
			mergeable = "CONFLICTING"
		}
	}
	var mergedAt, mergeCommit, closedAt any
	if item.closed {
		closedAt = f.timestamp
	}
	if item.mergedSHA != "" {
		mergedAt = f.timestamp
		mergeCommit = gql("Commit", record{"oid": item.mergedSHA, "id": "Commit_" + item.mergedSHA})
	}
	return gql("PullRequest", record{"id": node("PullRequest", item.id), "databaseId": item.id, "number": item.number, "title": item.title, "body": item.body, "state": pullState(item), "headRefName": item.head, "baseRefName": item.base, "headRefOid": item.headSHA, "baseRefOid": item.baseSHA, "headRef": gqlRef(item.head, item.headSHA), "baseRef": gqlRef(item.base, item.baseSHA), "mergeable": mergeable, "mergeStateStatus": map[string]string{"MERGEABLE": "CLEAN", "CONFLICTING": "DIRTY", "UNKNOWN": "UNKNOWN"}[mergeable], "merged": item.mergedSHA != "", "mergedAt": mergedAt, "mergeCommit": mergeCommit, "closed": item.closed, "closedAt": closedAt, "isDraft": item.draft, "author": f.gqlUser(f.login), "createdAt": f.timestamp, "updatedAt": f.timestamp,
		"files": gqlResolver(func(args record) (any, *fault) {
			repo.guard.RLock()
			defer repo.guard.RUnlock()
			files := []gqlObject{}
			for _, file := range repo.pullFiles(item) {
				files = append(files, gql("PullRequestChangedFile", record{"path": file["filename"], "additions": file["additions"], "deletions": file["deletions"], "changeType": strings.ToUpper(str(file, "status"))}))
			}
			return connection("PullRequestChangedFile", files, args), nil
		}),
	})
}
func (f *forge) graphMutate(name string, input record) (any, *fault) {
	if input == nil {
		return nil, fail(422, "Input is required")
	}
	payload := record{"clientMutationId": input["clientMutationId"]}
	if name == "createIssue" || name == "createPullRequest" {
		repo := f.byNode(str(input, "repositoryId"))
		if repo == nil {
			return nil, fail(404, "Repository not found")
		}
		repo.guard.Lock()
		defer repo.guard.Unlock()
		if name == "createIssue" {
			item, err := f.newIssue(repo, input)
			if err != nil {
				return nil, err
			}
			payload["issue"] = f.graphIssue(item)
			return gql("CreateIssuePayload", payload), nil
		}
		item, err := f.newPull(repo, record{"title": input["title"], "body": input["body"], "head": input["headRefName"], "base": input["baseRefName"], "draft": input["draft"]})
		if err != nil {
			return nil, err
		}
		payload["pullRequest"] = f.graphPull(repo, item)
		return gql("CreatePullRequestPayload", payload), nil
	}
	for _, repo := range f.reposList("") {
		repo.guard.Lock()
		value, err, found := f.graphMutateExisting(repo, name, input, payload)
		repo.guard.Unlock()
		if found {
			return value, err
		}
	}
	return nil, fail(404, "Subject not found")
}
func (f *forge) graphMutateExisting(repo *repository, name string, input, payload record) (any, *fault, bool) {
	if name == "mergePullRequest" {
		for _, item := range repo.pulls {
			if node("PullRequest", item.id) != str(input, "pullRequestId") {
				continue
			}
			_, err := f.mergePull(repo, item, record{"merge_method": input["mergeMethod"], "sha": input["expectedHeadOid"], "commit_title": input["commitHeadline"], "commit_message": input["commitBody"]})
			if err != nil {
				return nil, err, true
			}
			payload["pullRequest"] = f.graphPull(repo, item)
			return gql("MergePullRequestPayload", payload), nil, true
		}
	}
	for _, item := range repo.issues {
		id := node("Issue", item.id)
		if name == "addComment" && id == str(input, "subjectId") {
			comment, err := f.comment(repo, item, str(input, "body"))
			if err != nil {
				return nil, err, true
			}
			payload["commentEdge"] = gql("IssueCommentEdge", record{"node": f.graphComment(comment), "cursor": strconv.Itoa(len(item.comments))})
			return gql("AddCommentPayload", payload), nil, true
		}
		if (name == "closeIssue" || name == "reopenIssue") && id == str(input, "issueId") {
			item.state = "closed"
			kind := "CloseIssuePayload"
			if name == "reopenIssue" {
				item.state = "open"
				kind = "ReopenIssuePayload"
			}
			f.cacheIssue(repo, item)
			payload["issue"] = f.graphIssue(item)
			return gql(kind, payload), nil, true
		}
	}
	return nil, nil, false
}
