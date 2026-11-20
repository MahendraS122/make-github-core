package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

const mcpRevision = "2026-07-28"

type toolSpec struct {
	name, description  string
	required, optional []string
	readOnly           bool
}

var tools = []toolSpec{
	{"create_repo", "Create a repository for the authenticated user.", []string{"name"}, []string{"description", "private", "auto_init", "has_issues"}, false},
	{"get_repo", "Get a repository by owner and name.", []string{"owner", "repo"}, nil, true},
	{"create_issue", "Create an issue in a repository.", []string{"owner", "repo", "title"}, []string{"body", "labels"}, false},
	{"list_issues", "List repository issues filtered by state.", []string{"owner", "repo"}, []string{"state", "per_page", "page"}, true},
	{"create_pull_request", "Open a pull request from an existing head branch into a base branch.", []string{"owner", "repo", "title", "head", "base"}, []string{"body", "draft"}, false},
	{"merge_pull_request", "Merge a pull request into its base branch.", []string{"owner", "repo", "pull_number"}, []string{"merge_method", "commit_title", "commit_message", "sha"}, false},
}
var propertyTypes = map[string]string{"pull_number": "integer", "private": "boolean", "auto_init": "boolean", "has_issues": "boolean", "draft": "boolean", "per_page": "integer", "page": "integer", "labels": "array"}
var toolList = func() json.RawMessage {
	list := []record{}
	for _, tool := range tools {
		props := record{}
		for _, name := range append(append([]string{}, tool.required...), tool.optional...) {
			typ := propertyTypes[name]
			if typ == "" {
				typ = "string"
			}
			prop := record{"type": typ}
			if typ == "array" {
				prop["items"] = record{"type": "string"}
			}
			props[name] = prop
		}
		list = append(list, record{"name": tool.name, "description": tool.description, "inputSchema": record{"type": "object", "properties": props, "required": tool.required}, "annotations": record{"readOnlyHint": tool.readOnly}})
	}
	data, _ := json.Marshal(record{"tools": list})
	return data
}()

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  record          `json:"params"`
}

func rpcError(out http.ResponseWriter, status int, id json.RawMessage, code int, message string) {
	jsonReply(out, status, record{"jsonrpc": "2.0", "id": id, "error": record{"code": code, "message": message}})
}
func (f *forge) mcp(out http.ResponseWriter, req *http.Request) {
	out.Header().Set("Mcp-Session-Id", f.session)
	if req.Header.Get("Server-Protocol") == mcpRevision {
		out.Header().Set("Server-Protocol", mcpRevision)
		out.Header().Set("Server-Revision", mcpRevision)
	}
	if req.Method == "DELETE" {
		out.WriteHeader(204)
		return
	}
	if req.Method != "POST" {
		out.Header().Set("Allow", "POST, DELETE")
		problem(out, 405, "Method not allowed")
		return
	}
	data, err := io.ReadAll(io.LimitReader(req.Body, (2<<20)+1))
	if err != nil || len(data) > 2<<20 || !json.Valid(data) {
		rpcError(out, 400, nil, -32700, "Parse error")
		return
	}
	var call rpcRequest
	if json.Unmarshal(data, &call) != nil || !bytes.HasPrefix(bytes.TrimSpace(data), []byte{'{'}) || call.JSONRPC != "2.0" || call.Method == "" {
		rpcError(out, 400, nil, -32600, "Invalid request")
		return
	}
	if strings.HasPrefix(call.Method, "notifications/") {
		out.WriteHeader(202)
		return
	}
	var result any
	info := record{"name": "makefun-forge", "version": "1.0.0"}
	switch call.Method {
	case "initialize":
		version := str(call.Params, "protocolVersion")
		if version == "" {
			version = mcpRevision
		}
		result = record{"protocolVersion": version, "capabilities": record{"tools": record{}}, "serverInfo": info}
	case "server/discover":
		out.Header().Set("Server-Protocol", mcpRevision)
		out.Header().Set("Server-Revision", mcpRevision)
		result = record{"supportedVersions": []string{mcpRevision, "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}, "serverInfo": info, "capabilities": record{"tools": record{}}}
	case "ping":
		result = record{}
	case "tools/list":
		result = toolList
	case "tools/call":
		name := str(call.Params, "name")
		args := objectRecord(call.Params["arguments"])
		valid := false
		for _, tool := range tools {
			if tool.name != name {
				continue
			}
			valid = true
			for _, key := range tool.required {
				if args == nil || (propertyTypes[key] == "integer" && integer(args[key]) <= 0) || (propertyTypes[key] != "integer" && strings.TrimSpace(str(args, key)) == "") {
					rpcError(out, 200, call.ID, -32602, "Missing or invalid argument: "+key)
					return
				}
			}
			break
		}
		if !valid {
			rpcError(out, 200, call.ID, -32602, "Unknown tool: "+name)
			return
		}
		value, problem := f.callTool(name, args)
		content := ""
		if problem != nil {
			content = problem.message
		} else {
			encoded, _ := json.Marshal(value)
			content = string(encoded)
		}
		result = record{"content": []record{{"type": "text", "text": content}}, "isError": problem != nil}
	default:
		rpcError(out, 200, call.ID, -32601, "Method not found")
		return
	}
	jsonReply(out, 200, record{"jsonrpc": "2.0", "id": call.ID, "result": result})
}
func (f *forge) callTool(name string, args record) (any, *fault) {
	if name == "create_repo" {
		repo, err := f.createRepo(f.login, args)
		if err != nil {
			return nil, err
		}
		repo.guard.RLock()
		defer repo.guard.RUnlock()
		return f.repoView(repo), nil
	}
	repo := f.repo(str(args, "owner"), str(args, "repo"))
	if repo == nil {
		return nil, fail(404, "Repository not found")
	}
	if name == "get_repo" || name == "list_issues" {
		repo.guard.RLock()
		defer repo.guard.RUnlock()
	} else {
		repo.guard.Lock()
		defer repo.guard.Unlock()
	}
	switch name {
	case "get_repo":
		return f.repoView(repo), nil
	case "create_issue":
		item, err := f.newIssue(repo, args)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(item.cached), nil
	case "list_issues":
		state := strings.ToLower(str(args, "state"))
		list := repo.issueList(state, false)
		size := integer(args["per_page"])
		if size <= 0 {
			size = 30
		}
		size = min(size, 100)
		page := max(1, integer(args["page"]))
		start := len(list)
		if page <= len(list)/size+1 {
			start = (page - 1) * size
		}
		result := []json.RawMessage{}
		for _, item := range list[start:min(len(list), start+size)] {
			result = append(result, item.cached)
		}
		return result, nil
	case "create_pull_request":
		item, err := f.newPull(repo, args)
		if err != nil {
			return nil, err
		}
		return f.pullView(repo, item), nil
	case "merge_pull_request":
		item := repo.pulls[integer(args["pull_number"])]
		if item == nil {
			return nil, fail(404, "Pull request not found")
		}
		return f.mergePull(repo, item, args)
	}
	return nil, fail(422, "Unknown tool")
}
