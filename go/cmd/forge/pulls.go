package main

import (
	"bytes"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

type pull struct {
	id                                                         int64
	number                                                     int
	title, body, head, base, headSHA, baseSHA, fork, mergedSHA string
	closed, draft                                              bool
}

func (repo *repository) mergeBase(base, head string) string {
	ancestors := map[string]bool{}
	queue := []string{base}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if ancestors[oid] {
			continue
		}
		ancestors[oid] = true
		if obj := repo.objects[oid]; obj != nil {
			queue = append(queue, obj.parents...)
		}
	}
	queue = []string{head}
	seen := map[string]bool{}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if ancestors[oid] {
			return oid
		}
		if obj := repo.objects[oid]; obj != nil {
			queue = append(queue, obj.parents...)
		}
	}
	return ""
}
func (f *forge) newPull(repo *repository, input record) (*pull, *fault) {
	head, base, title := str(input, "head"), str(input, "base"), str(input, "title")
	if owner, branch, ok := strings.Cut(head, ":"); ok {
		if !strings.EqualFold(owner, repo.owner) {
			return nil, fail(422, "Unknown head repository")
		}
		head = branch
	}
	head = strings.TrimPrefix(head, "refs/heads/")
	base = strings.TrimPrefix(base, "refs/heads/")
	headSHA, baseSHA := repo.refs["refs/heads/"+head], repo.refs["refs/heads/"+base]
	if headSHA == "" || baseSHA == "" || strings.TrimSpace(title) == "" || repo.objects.ancestor(headSHA, baseSHA) {
		return nil, fail(422, "No commits between valid head and base branches")
	}
	fork := repo.mergeBase(baseSHA, headSHA)
	if fork == "" {
		return nil, fail(422, "Branches do not share history")
	}
	for _, item := range repo.pulls {
		if !item.closed && item.head == head && item.base == base {
			return nil, fail(422, "A pull request already exists")
		}
	}
	repo.nextNumber++
	item := &pull{id: f.ids.Add(1), number: repo.nextNumber, title: title, body: str(input, "body"), head: head, base: base, headSHA: headSHA, baseSHA: baseSHA, fork: fork, draft: boolean(input, "draft", false)}
	repo.pulls[item.number] = item
	return item, nil
}
func entryEqual(left, right treeEntry) bool { return left.oid == right.oid && left.mode == right.mode }
func (repo *repository) mergeFiles(item *pull) (map[string]treeEntry, map[string][]byte, bool) {
	current := repo.refs["refs/heads/"+item.base]
	if current == "" {
		return nil, nil, false
	}
	fork := repo.mergeBase(current, item.headSHA)
	if fork == "" {
		return nil, nil, false
	}
	original, head, target := repo.files(fork), repo.files(item.headSHA), repo.files(current)
	var synthesized map[string][]byte
	paths := map[string]bool{}
	for path := range original {
		paths[path] = true
	}
	for path := range head {
		paths[path] = true
	}
	for path := range paths {
		before, after, live := original[path], head[path], target[path]
		if entryEqual(before, after) {
			continue
		}
		if !entryEqual(live, before) && !entryEqual(live, after) {
			if before.mode == after.mode && before.mode == live.mode && repo.objects[before.oid] != nil && repo.objects[after.oid] != nil && repo.objects[live.oid] != nil {
				merged, ok := mergeText(repo.objects[before.oid].data, repo.objects[live.oid].data, repo.objects[after.oid].data)
				if ok {
					after.oid = objectID(3, merged)
					target[path] = after
					if synthesized == nil {
						synthesized = map[string][]byte{}
					}
					synthesized[after.oid] = merged
					continue
				}
			}
			return nil, nil, false
		}
		if after.oid == "" {
			delete(target, path)
		} else {
			target[path] = after
		}
	}
	for path := range target {
		for index := strings.IndexByte(path, '/'); index >= 0; {
			if target[path[:index]].oid != "" {
				return nil, nil, false
			}
			next := strings.IndexByte(path[index+1:], '/')
			if next < 0 {
				break
			}
			index += next + 1
		}
	}
	return target, synthesized, true
}

func (f *forge) mergePull(repo *repository, item *pull, input record) (record, *fault) {
	if item.closed || item.draft {
		return nil, fail(405, "Pull request is not mergeable")
	}
	if expected := str(input, "sha"); expected != "" && expected != item.headSHA {
		return nil, fail(409, "Head SHA does not match")
	}
	files, synthesized, clean := repo.mergeFiles(item)
	if !clean {
		return nil, fail(405, "Pull request is not mergeable")
	}
	for _, data := range synthesized {
		repo.objects.put(3, data)
	}
	ref := "refs/heads/" + item.base
	parents := []string{repo.refs[ref]}
	method := strings.ToLower(str(input, "merge_method"))
	if method != "squash" {
		parents = append(parents, item.headSHA)
	}
	title := str(input, "commit_title")
	if title == "" {
		title = item.title + " (#" + strconv.Itoa(item.number) + ")"
	}
	if body := str(input, "commit_message"); body != "" {
		title += "\n\n" + body
	}
	oid := f.commit(repo, repo.objects.makeTree(files), parents, title)
	repo.refs[ref] = oid
	repo.changed()
	item.mergedSHA = oid
	item.closed = true
	return record{"sha": oid, "merged": true, "message": "Pull Request successfully merged"}, nil
}
func (f *forge) pullView(repo *repository, item *pull) record {
	number := strconv.Itoa(item.number)
	path := repoURL(repo) + "/pulls/" + number
	full := repo.owner + "/" + repo.name
	state := "open"
	var mergedAt, closedAt, sha any
	if item.closed {
		state = "closed"
		closedAt = f.timestamp
	}
	if item.mergedSHA != "" {
		mergedAt = f.timestamp
		sha = item.mergedSHA
	}
	ref := func(branch, oid string) record {
		return record{"label": repo.owner + ":" + branch, "ref": branch, "sha": oid, "repo": f.repoView(repo), "user": user(f.login)}
	}
	mergeable := false
	mergeState := "dirty"
	if !item.closed {
		_, _, mergeable = repo.mergeFiles(item)
		if mergeable {
			mergeState = "clean"
		}
	} else if item.mergedSHA != "" {
		mergeState = "unknown"
	}
	return record{"id": item.id, "node_id": node("PullRequest", item.id), "number": item.number, "title": item.title, "body": item.body, "state": state, "user": user(f.login), "url": path, "html_url": "/" + full + "/pull/" + number, "issue_url": repoURL(repo) + "/issues/" + number, "diff_url": "/" + full + "/pull/" + number + ".diff", "patch_url": "/" + full + "/pull/" + number + ".patch", "created_at": f.timestamp, "updated_at": f.timestamp, "closed_at": closedAt, "merged_at": mergedAt, "merged": item.mergedSHA != "", "merge_commit_sha": sha, "mergeable": mergeable, "mergeable_state": mergeState, "head": ref(item.head, item.headSHA), "base": ref(item.base, item.baseSHA), "draft": item.draft, "locked": false, "labels": []record{}, "assignees": []record{}}
}
func (repo *repository) pullList(state string) []*pull {
	if state == "" {
		state = "open"
	}
	list := make([]*pull, 0, len(repo.pulls))
	for number := repo.nextNumber; number > 0; number-- {
		item := repo.pulls[number]
		if item != nil && (state == "all" || state == "open" && !item.closed || state == "closed" && item.closed || state == "merged" && item.mergedSHA != "") {
			list = append(list, item)
		}
	}
	return list
}
func (f *forge) restPulls(out http.ResponseWriter, req *http.Request, repo *repository, parts []string, input record) {
	if len(parts) == 0 {
		if req.Method == "POST" {
			item, err := f.newPull(repo, input)
			if err != nil {
				respond(out, 201, nil, err)
			} else {
				jsonReply(out, 201, f.pullView(repo, item))
			}
			return
		}
		if req.Method == "GET" {
			query := req.URL.Query()
			list := repo.pullList(query.Get("state"))
			list = slices.DeleteFunc(list, func(item *pull) bool {
				return query.Get("base") != "" && item.base != query.Get("base") || query.Get("head") != "" && repo.owner+":"+item.head != query.Get("head") && item.head != query.Get("head")
			})
			views := []record{}
			for _, item := range paginate(out, req, list, 30) {
				views = append(views, f.pullView(repo, item))
			}
			jsonReply(out, 200, views)
			return
		}
	} else if item := repo.pulls[integer(parts[0])]; item != nil {
		if len(parts) == 1 && req.Method == "GET" {
			jsonReply(out, 200, f.pullView(repo, item))
			return
		}
		if len(parts) == 2 {
			if parts[1] == "files" && req.Method == "GET" {
				jsonReply(out, 200, paginate(out, req, repo.pullFiles(item), 30))
				return
			}
			if parts[1] == "merge" && (req.Method == "PUT" || req.Method == "POST") {
				value, err := f.mergePull(repo, item, input)
				respond(out, 200, value, err)
				return
			}
		}
	}
	missing(out)
}
func (repo *repository) pullFiles(item *pull) []record {
	before, after := repo.files(item.fork), repo.files(item.headSHA)
	paths := map[string]bool{}
	for path := range before {
		paths[path] = true
	}
	for path := range after {
		paths[path] = true
	}
	list := []record{}
	for _, path := range slices.Sorted(mapKeys(paths)) {
		old, next := before[path], after[path]
		if entryEqual(old, next) {
			continue
		}
		status := "modified"
		var left, right []byte
		if old.oid == "" {
			status = "added"
		} else if obj := repo.objects[old.oid]; obj != nil {
			left = obj.data
		}
		if next.oid == "" {
			status = "removed"
		} else if obj := repo.objects[next.oid]; obj != nil {
			right = obj.data
		}
		oid := next.oid
		if oid == "" {
			oid = old.oid
		}
		encoded := escapePath(path)
		full := "/" + repo.owner + "/" + repo.name
		view := record{"filename": path, "status": status, "sha": oid, "additions": 0, "deletions": 0, "changes": 0, "blob_url": full + "/blob/" + item.headSHA + "/" + encoded, "raw_url": full + "/raw/" + item.headSHA + "/" + encoded, "contents_url": repoURL(repo) + "/contents/" + encoded + "?ref=" + item.headSHA}
		if !bytes.Contains(left, []byte{0}) && !bytes.Contains(right, []byte{0}) {
			patch, adds, deletes := unified(left, right)
			view["additions"], view["deletions"], view["changes"] = adds, deletes, adds+deletes
			if patch != "" {
				view["patch"] = patch
			}
		}
		list = append(list, view)
	}
	return list
}

type diffLine struct {
	mark byte
	line string
}

func splitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	out := strings.SplitAfter(string(data), "\n")
	if out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

type lineEdit struct {
	start, end  int
	replacement []string
}

func lineEdits(original, modified []string) []lineEdit {
	var result []lineEdit
	position := 0
	var pending *lineEdit
	for _, line := range edits(original, modified) {
		if line.mark == ' ' {
			if pending != nil {
				result = append(result, *pending)
				pending = nil
			}
			position++
			continue
		}
		if pending == nil {
			pending = &lineEdit{start: position, end: position}
		}
		if line.mark == '-' {
			position++
			pending.end++
		} else {
			pending.replacement = append(pending.replacement, line.line)
		}
	}
	if pending != nil {
		result = append(result, *pending)
	}
	return result
}
func mergeText(original, left, right []byte) ([]byte, bool) {
	for _, data := range [][]byte{original, left, right} {
		if bytes.IndexByte(data, 0) >= 0 {
			return nil, false
		}
	}
	base := splitLines(original)
	a, b := lineEdits(base, splitLines(left)), lineEdits(base, splitLines(right))
	var merged strings.Builder
	position := 0
	for len(a)+len(b) > 0 {
		var next lineEdit
		if len(a) > 0 && len(b) > 0 && (a[0].start == b[0].start || a[0].start < b[0].end && b[0].start < a[0].end) {
			if a[0].start != b[0].start || a[0].end != b[0].end || !slices.Equal(a[0].replacement, b[0].replacement) {
				return nil, false
			}
			next = a[0]
			a, b = a[1:], b[1:]
		} else if len(b) == 0 || len(a) > 0 && a[0].start < b[0].start {
			next = a[0]
			a = a[1:]
		} else {
			next = b[0]
			b = b[1:]
		}
		if next.start < position {
			return nil, false
		}
		for _, line := range base[position:next.start] {
			merged.WriteString(line)
		}
		for _, line := range next.replacement {
			merged.WriteString(line)
		}
		position = next.end
	}
	for _, line := range base[position:] {
		merged.WriteString(line)
	}
	return []byte(merged.String()), true
}
func edits(left, right []string) []diffLine {
	var result []diffLine
	for len(left) > 0 && len(right) > 0 && left[0] == right[0] {
		result = append(result, diffLine{' ', left[0]})
		left, right = left[1:], right[1:]
	}
	suffix := 0
	for suffix < len(left) && suffix < len(right) && left[len(left)-1-suffix] == right[len(right)-1-suffix] {
		suffix++
	}
	tail := left[len(left)-suffix:]
	left, right = left[:len(left)-suffix], right[:len(right)-suffix]
	width := len(right) + 1
	if len(left) > 0 && len(right) > 0 && (len(left)+1) <= 500000/width {
		table := make([]int, (len(left)+1)*width)
		for row := len(left) - 1; row >= 0; row-- {
			for col := len(right) - 1; col >= 0; col-- {
				index := row*width + col
				if left[row] == right[col] {
					table[index] = 1 + table[index+width+1]
				} else {
					table[index] = max(table[index+width], table[index+1])
				}
			}
		}
		row, col := 0, 0
		for row < len(left) || col < len(right) {
			if row < len(left) && col < len(right) && left[row] == right[col] {
				result = append(result, diffLine{' ', left[row]})
				row++
				col++
			} else if row < len(left) && (col == len(right) || table[(row+1)*width+col] >= table[row*width+col+1]) {
				result = append(result, diffLine{'-', left[row]})
				row++
			} else {
				result = append(result, diffLine{'+', right[col]})
				col++
			}
		}
	} else {
		for _, line := range left {
			result = append(result, diffLine{'-', line})
		}
		for _, line := range right {
			result = append(result, diffLine{'+', line})
		}
	}
	for _, line := range tail {
		result = append(result, diffLine{' ', line})
	}
	return result
}
func unified(left, right []byte) (string, int, int) {
	diff := edits(splitLines(left), splitLines(right))
	var patch strings.Builder
	adds, deletes := 0, 0
	old, new := 1, 1
	for pos := 0; pos < len(diff); {
		change := pos
		for change < len(diff) && diff[change].mark == ' ' {
			change++
		}
		if change == len(diff) {
			break
		}
		start := max(pos, change-3)
		old += start - pos
		new += start - pos
		last := change
		for scan := change + 1; scan < len(diff) && scan <= last+7; scan++ {
			if diff[scan].mark != ' ' {
				last = scan
			}
		}
		end := min(len(diff), last+4)
		oldCount, newCount := 0, 0
		for _, line := range diff[start:end] {
			if line.mark != '+' {
				oldCount++
			}
			if line.mark != '-' {
				newCount++
			}
		}
		fmt.Fprintf(&patch, "@@ -%s +%s @@\n", diffRange(old, oldCount), diffRange(new, newCount))
		for _, line := range diff[start:end] {
			patch.WriteByte(line.mark)
			patch.WriteString(line.line)
			if !strings.HasSuffix(line.line, "\n") {
				patch.WriteString("\n\\ No newline at end of file\n")
			}
			if line.mark == '+' {
				adds++
			}
			if line.mark == '-' {
				deletes++
			}
		}
		old += oldCount
		new += newCount
		pos = end
	}
	return strings.TrimSuffix(patch.String(), "\n"), adds, deletes
}
func diffRange(start, count int) string {
	if count == 1 {
		return strconv.Itoa(start)
	}
	if count == 0 {
		start--
	}
	return strconv.Itoa(start) + "," + strconv.Itoa(count)
}
