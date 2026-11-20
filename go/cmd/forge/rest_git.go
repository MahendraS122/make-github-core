package main

import (
	"encoding/base64"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
)

func (repo *repository) files(oid string) map[string]treeEntry {
	files := map[string]treeEntry{}
	if obj := repo.objects[oid]; obj != nil && obj.kind == 1 {
		repo.objects.flat(obj.target, "", files)
	}
	return files
}
func (f *forge) commitView(repo *repository, oid string) record {
	obj := repo.objects[oid]
	commit := f.rawCommit(repo, oid)
	return record{"sha": oid, "node_id": "Commit_" + oid, "url": repoURL(repo) + "/commits/" + oid, "html_url": "/" + repo.owner + "/" + repo.name + "/commit/" + oid, "commit": commit, "parents": commit["parents"], "author": user(f.login), "committer": user(f.login), "tree": commit["tree"], "message": obj.message}
}
func identity(value string) record {
	name, rest, _ := strings.Cut(value, " <")
	email, tail, _ := strings.Cut(rest, "> ")
	fields := strings.Fields(tail)
	var date string
	if len(fields) > 0 {
		epoch, _ := strconv.ParseInt(fields[0], 10, 64)
		date = time.Unix(epoch, 0).UTC().Format(time.RFC3339)
	}
	return record{"name": name, "email": email, "date": date}
}
func (f *forge) rawCommit(repo *repository, oid string) record {
	obj := repo.objects[oid]
	parents := []record{}
	for _, parent := range obj.parents {
		parents = append(parents, f.commitPointer(repo, parent))
	}
	view := record{"sha": oid, "node_id": "Commit_" + oid, "message": obj.message, "parents": parents, "tree": record{"sha": obj.target, "url": repoURL(repo) + "/git/trees/" + obj.target}, "url": repoURL(repo) + "/git/commits/" + oid, "verification": record{"verified": false, "reason": "unsigned", "signature": nil, "payload": nil}}
	head, _, _ := strings.Cut(string(obj.data), "\n\n")
	for line := range strings.SplitSeq(head, "\n") {
		kind, person, _ := strings.Cut(line, " ")
		if kind == "author" || kind == "committer" {
			view[kind] = identity(person)
		}
	}
	return view
}
func (f *forge) restGit(out http.ResponseWriter, req *http.Request, repo *repository, parts []string, input record) {
	if len(parts) == 0 {
		missing(out)
		return
	}
	resource := parts[0]
	suffix := strings.Join(parts[1:], "/")
	method := req.Method
	refView := func(ref, oid string) record {
		kind := "commit"
		if obj := repo.objects[oid]; obj != nil {
			kind = kinds[obj.kind]
		}
		return record{"ref": ref, "node_id": "Ref_" + ref, "url": repoURL(repo) + "/git/refs/" + strings.TrimPrefix(ref, "refs/"), "object": record{"sha": oid, "type": kind, "url": repoURL(repo) + "/git/" + kind + "s/" + oid}}
	}
	if resource == "refs" || resource == "matching-refs" {
		ref := suffix
		if !strings.HasPrefix(ref, "refs/") {
			ref = "refs/" + ref
		}
		if method == "GET" {
			if resource == "refs" && suffix != "" {
				if oid := repo.refs[ref]; oid != "" {
					jsonReply(out, 200, refView(ref, oid))
					return
				}
				missing(out)
				return
			}
			list := []record{}
			for _, name := range refNames(repo.refs) {
				if strings.HasPrefix(name, ref) {
					list = append(list, refView(name, repo.refs[name]))
				}
			}
			jsonReply(out, 200, list)
			return
		}
		if method == "POST" && suffix == "" {
			ref = str(input, "ref")
			oid := str(input, "sha")
			if !validRef(ref) || repo.refs[ref] != "" || repo.objects[oid] == nil {
				problem(out, 422, "Invalid or duplicate reference")
				return
			}
			repo.refs[ref] = oid
			repo.changed()
			jsonReply(out, 201, refView(ref, oid))
			return
		}
		if method == "PATCH" && suffix != "" {
			old, oid := repo.refs[ref], str(input, "sha")
			if old == "" {
				missing(out)
				return
			}
			if repo.objects[oid] == nil || !repo.objects.ancestor(old, oid) {
				problem(out, 422, "Update is not a fast forward")
				return
			}
			repo.refs[ref] = oid
			repo.changed()
			jsonReply(out, 200, refView(ref, oid))
			return
		}
		if method == "DELETE" && suffix != "" {
			if repo.refs[ref] == "" {
				missing(out)
				return
			}
			delete(repo.refs, ref)
			repo.changed()
			out.WriteHeader(204)
			return
		}
	}
	if method != "GET" || suffix == "" {
		missing(out)
		return
	}
	oid := repo.objects.peel(repo.resolve(suffix))
	obj := repo.objects[oid]
	if obj == nil {
		missing(out)
		return
	}
	switch resource {
	case "commits":
		if obj.kind == 1 {
			jsonReply(out, 200, f.rawCommit(repo, oid))
			return
		}
	case "trees":
		if obj.kind == 1 {
			oid = obj.target
			obj = repo.objects[oid]
		}
		if obj == nil || obj.kind != 2 {
			break
		}
		entries := []record{}
		recursive := req.URL.Query().Has("recursive")
		var visit func(*object, string)
		visit = func(tree *object, prefix string) {
			if tree == nil || len(prefix) > 4096 {
				return
			}
			for _, entry := range tree.entries {
				kind := "blob"
				mode := entry.mode
				switch mode {
				case "40000", "040000":
					mode = "040000"
					kind = "tree"
				case "160000":
					kind = "commit"
				}
				view := record{"path": prefix + entry.name, "mode": mode, "type": kind, "sha": entry.oid, "url": repoURL(repo) + "/git/" + kind + "s/" + entry.oid}
				child := repo.objects[entry.oid]
				if kind == "blob" && child != nil {
					view["size"] = len(child.data)
				}
				entries = append(entries, view)
				if recursive && kind == "tree" {
					visit(child, prefix+entry.name+"/")
				}
			}
		}
		visit(obj, "")
		jsonReply(out, 200, record{"sha": oid, "url": repoURL(repo) + "/git/trees/" + oid, "tree": entries, "truncated": false})
		return
	case "blobs":
		if obj.kind == 3 {
			jsonReply(out, 200, record{"sha": oid, "size": len(obj.data), "encoding": "base64", "content": base64.StdEncoding.EncodeToString(obj.data), "url": repoURL(repo) + "/git/blobs/" + oid})
			return
		}
	}
	missing(out)
}

func (f *forge) contentView(repo *repository, name string, entry treeEntry, withData bool) record {
	url := repoURL(repo) + "/contents/" + escapePath(name)
	html := "/" + repo.owner + "/" + repo.name + "/blob/" + repo.branch + "/" + escapePath(name)
	view := record{"name": path.Base(name), "path": name, "sha": entry.oid, "url": url, "html_url": html, "size": 0, "type": "file", "download_url": nil}
	if entry.mode == "40000" || entry.mode == "040000" {
		view["type"] = "dir"
		view["git_url"] = repoURL(repo) + "/git/trees/" + entry.oid
	} else {
		view["git_url"] = repoURL(repo) + "/git/blobs/" + entry.oid
		if obj := repo.objects[entry.oid]; obj != nil {
			view["size"] = len(obj.data)
			if withData {
				view["encoding"] = "base64"
				view["content"] = base64.StdEncoding.EncodeToString(obj.data) + "\n"
			}
		}
	}
	view["_links"] = record{"self": url, "git": view["git_url"], "html": html}
	return view
}
func (f *forge) contents(out http.ResponseWriter, req *http.Request, repo *repository, name string, input record) {
	if name != "" && (len(name) > 4096 || path.Clean(name) != name || name == "." || name == ".." || strings.HasPrefix(name, "../") || strings.ContainsRune(name, 0)) {
		problem(out, 422, "Invalid path")
		return
	}
	if req.Method == "GET" {
		oid := repo.objects.peel(repo.resolve(req.URL.Query().Get("ref")))
		commit := repo.objects[oid]
		if commit == nil || commit.kind != 1 {
			missing(out)
			return
		}
		tree := repo.objects[commit.target]
		if name != "" {
			full := ""
			for component := range strings.SplitSeq(name, "/") {
				if tree == nil || tree.kind != 2 {
					missing(out)
					return
				}
				index := slices.IndexFunc(tree.entries, func(entry treeEntry) bool { return entry.name == component })
				if index < 0 {
					missing(out)
					return
				}
				entry := tree.entries[index]
				if full != "" {
					full += "/"
				}
				full += component
				if full == name && entry.mode != "40000" && entry.mode != "040000" {
					jsonReply(out, 200, f.contentView(repo, name, entry, true))
					return
				}
				tree = repo.objects[entry.oid]
			}
		}
		if tree == nil || tree.kind != 2 {
			missing(out)
			return
		}
		list := []record{}
		for _, entry := range tree.entries {
			full := entry.name
			if name != "" {
				full = name + "/" + full
			}
			list = append(list, f.contentView(repo, full, entry, false))
		}
		jsonReply(out, 200, list)
		return
	}
	if name == "" || (req.Method != "PUT" && req.Method != "DELETE") {
		missing(out)
		return
	}
	branch := str(input, "branch")
	if branch == "" {
		branch = repo.branch
	}
	ref := "refs/heads/" + strings.TrimPrefix(branch, "refs/heads/")
	current := repo.refs[ref]
	if !validRef(ref) || (current == "" && ref != repo.head()) {
		problem(out, 404, "Branch not found")
		return
	}
	files := repo.files(current)
	previous, exists := files[name]
	provided := str(input, "sha")
	if exists && provided != previous.oid || !exists && provided != "" {
		problem(out, 409, "SHA does not match current content")
		return
	}
	status := 200
	var content any
	if req.Method == "DELETE" {
		if !exists {
			missing(out)
			return
		}
		delete(files, name)
	} else {
		encoded, ok := input["content"].(string)
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if !ok || err != nil {
			problem(out, 422, "Content must be base64 encoded")
			return
		}
		for file := range files {
			if strings.HasPrefix(name, file+"/") || strings.HasPrefix(file, name+"/") {
				problem(out, 409, "Path conflicts with existing content")
				return
			}
		}
		entry := treeEntry{"100644", path.Base(name), repo.objects.put(3, decoded)}
		if exists {
			entry.mode = previous.mode
		} else {
			status = 201
		}
		files[name] = entry
		content = f.contentView(repo, name, entry, true)
	}
	message := str(input, "message")
	if message == "" {
		message = "Update " + name
	}
	oid := f.commit(repo, repo.objects.makeTree(files), []string{current}, message)
	repo.refs[ref] = oid
	repo.changed()
	jsonReply(out, status, record{"content": content, "commit": f.rawCommit(repo, oid)})
}
