package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
)

const upload = "git-upload-pack"
const receive = "git-receive-pack"
const digits = "0123456789abcdef"

func packet(data []byte, payload string) []byte {
	size := len(payload) + 4
	return append(append(data, digits[size>>12], digits[size>>8&15], digits[size>>4&15], digits[size&15]), payload...)
}
func sideband(data, payload []byte) []byte {
	for len(payload) > 0 {
		length := min(65515, len(payload))
		size := length + 5
		data = append(data, digits[size>>12], digits[size>>8&15], digits[size>>4&15], digits[size&15], 1)
		data = append(data, payload[:length]...)
		payload = payload[length:]
	}
	return data
}
func unpackLine(data []byte) (string, int, []byte) {
	if len(data) < 4 {
		return "", -1, nil
	}
	length := 0
	for _, digit := range data[:4] {
		index := strings.IndexByte(digits, digit)
		if index < 0 {
			return "", -1, nil
		}
		length = length*16 + index
	}
	if length < 4 {
		return "", length, data[4:]
	}
	if length > len(data) {
		return "", -1, nil
	}
	return string(data[4:length]), length, data[length:]
}
func refNames(refs map[string]string) []string { return slices.Sorted(mapKeys(refs)) }
func (repo *repository) advertise(service string, v2 bool) []byte {
	response := append(packet(nil, "# service="+service+"\n"), "0000"...)
	if v2 {
		for _, line := range []string{"version 2\n", "agent=forge/1.0\n", "ls-refs=unborn\n", "fetch=shallow\n", "object-format=sha1\n"} {
			response = packet(response, line)
		}
		return append(response, "0000"...)
	}
	caps := "report-status side-band-64k delete-refs ofs-delta agent=forge/1.0"
	if service == upload {
		caps = "multi_ack_detailed side-band-64k thin-pack ofs-delta shallow no-progress include-tag symref=HEAD:" + repo.head() + " agent=forge/1.0"
	}
	first := true
	add := func(name, oid string) {
		line := oid + " " + name
		if first {
			line += "\x00" + caps
			first = false
		}
		response = packet(response, line+"\n")
	}
	if oid := repo.refs[repo.head()]; oid != "" && service == upload {
		add("HEAD", oid)
	}
	for _, name := range refNames(repo.refs) {
		oid := repo.refs[name]
		add(name, oid)
		if service == upload && strings.HasPrefix(name, "refs/tags/") {
			if peeled := repo.objects.peel(oid); peeled != oid {
				add(name+"^{}", peeled)
			}
		}
	}
	if first {
		add("capabilities^{}", zero)
	}
	return append(response, "0000"...)
}
func (f *forge) gitHTTP(out http.ResponseWriter, req *http.Request, parts []string) {
	repo := f.repo(parts[0], strings.TrimSuffix(parts[1], ".git"))
	if repo == nil {
		missing(out)
		return
	}
	path := strings.Join(parts[2:], "/")
	v2 := strings.Contains(req.Header.Get("Git-Protocol"), "version=2")
	out.Header().Set("Cache-Control", "no-cache")
	if req.Method == "GET" && path == "info/refs" {
		service := req.URL.Query().Get("service")
		if service != upload && service != receive {
			problem(out, 400, "Unsupported service")
			return
		}
		out.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
		adKey := service
		if v2 && service == upload {
			adKey += "-v2"
		}
		repo.guard.RLock()
		ad := repo.ad[adKey]
		repo.guard.RUnlock()
		if ad == nil {
			repo.guard.Lock()
			ad = repo.ad[adKey]
			if ad == nil {
				ad = repo.advertise(service, v2 && service == upload)
				if repo.ad == nil {
					repo.ad = make(map[string][]byte, 3)
				}
				repo.ad[adKey] = ad
			}
			repo.guard.Unlock()
		}
		out.Write(ad)
		return
	}
	if req.Method != "POST" || (path != upload && path != receive) {
		missing(out)
		return
	}
	var reader io.Reader = req.Body
	if req.Header.Get("Content-Encoding") == "gzip" {
		gzipReader, err := gzip.NewReader(reader)
		if err != nil {
			problem(out, 400, "Invalid gzip body")
			return
		}
		defer gzipReader.Close()
		reader = gzipReader
	}
	limit := int64(96 << 20)
	if path == upload {
		limit = 2 << 20
	}
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil || int64(len(body)) > limit {
		problem(out, 400, "Invalid request body")
		return
	}
	var response []byte
	if path == receive {
		response = repo.receive(body)
	} else {
		repo.guard.RLock()
		response = repo.upload(body, v2)
		repo.guard.RUnlock()
	}
	out.Header().Set("Content-Type", "application/x-"+path+"-result")
	out.Header().Set("Content-Length", strconv.Itoa(len(response)))
	out.Write(response)
}

func validRef(ref string) bool {
	if !strings.HasPrefix(ref, "refs/") || strings.HasSuffix(ref, ".") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.ContainsAny(ref, " ~^:?*[\\") {
		return false
	}
	for part := range strings.SplitSeq(ref, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	for _, char := range ref {
		if char < 32 || char == 127 {
			return false
		}
	}
	return true
}
func (repo *repository) receive(body []byte) []byte {
	type update struct{ old, next, ref string }
	var commands []update
	var caps string
	var response []byte
	unpack := "ok"
	changed := false
	for {
		line, length, rest := unpackLine(body)
		body = rest
		if length == 0 {
			break
		}
		if length < 4 {
			unpack = "invalid commands"
			break
		}
		if len(commands) == 0 {
			line, caps, _ = strings.Cut(line, "\x00")
		}
		parts := strings.Fields(line)
		if len(parts) != 3 || len(parts[0]) != 40 || len(parts[1]) != 40 {
			unpack = "invalid commands"
			break
		}
		commands = append(commands, update{parts[0], parts[1], parts[2]})
	}
	if unpack == "ok" && len(body) > 0 {
		incoming, err := readPackLookup(body, func(oid string) *object {
			repo.guard.RLock()
			obj := repo.objects[oid]
			repo.guard.RUnlock()
			return obj
		})
		repo.guard.Lock()
		if err != nil {
			unpack = err.Error()
		} else {
			for oid, obj := range incoming {
				if repo.objects[oid] == nil {
					repo.objects[oid] = obj
				}
			}
		}
	} else {
		repo.guard.Lock()
	}
	defer repo.guard.Unlock()
	response = packet(response, "unpack "+unpack+"\n")
	for _, command := range commands {
		current := repo.refs[command.ref]
		if current == "" {
			current = zero
		}
		reason := ""
		obj := repo.objects[command.next]
		switch {
		case unpack != "ok":
			reason = "unpacker error"
		case !validRef(command.ref):
			reason = "invalid ref"
		case command.old != current:
			reason = "stale ref"
		case command.next == zero:
			delete(repo.refs, command.ref)
			changed = true
		case obj == nil:
			reason = "missing object"
		case strings.HasPrefix(command.ref, "refs/heads/") && obj.kind != 1:
			reason = "branch must point to a commit"
		case current != zero && current != command.next && !repo.objects.ancestor(current, command.next):
			reason = "non-fast-forward"
		default:
			repo.refs[command.ref] = command.next
			changed = true
		}
		if reason == "" {
			response = packet(response, "ok "+command.ref+"\n")
		} else {
			response = packet(response, "ng "+command.ref+" "+reason+"\n")
		}
	}
	if changed {
		repo.changed()
	}
	response = append(response, "0000"...)
	if slices.Contains(strings.Fields(caps), "side-band-64k") {
		response = append(sideband(nil, response), "0000"...)
	}
	return response
}

type packResponse struct {
	once sync.Once
	data []byte
}

func (repo *repository) upload(body []byte, v2 bool) []byte {
	first, _, _ := unpackLine(body)
	v2 = v2 || strings.HasPrefix(first, "command=")
	cacheable := len(body) < 1024 && ((bytes.Contains(body, []byte("done")) && !bytes.Contains(body, []byte("have "))) || first == "command=ls-refs\n")
	if cacheable {
		cacheKey := string(body)
		if v2 {
			cacheKey = "v2:" + cacheKey
		}
		cached, ok := repo.packCache.Load(cacheKey)
		if !ok {
			cached, _ = repo.packCache.LoadOrStore(cacheKey, &packResponse{})
		}
		response := cached.(*packResponse)
		response.once.Do(func() { response.data = repo.buildUpload(body, v2) })
		return response.data
	}
	return repo.buildUpload(body, v2)
}

func (repo *repository) buildUpload(body []byte, v2 bool) []byte {
	var wants, haves, prefixes []string
	shallow := map[string]bool{}
	depth := 0
	var done, band, ofs, peel, symrefs, unborn, ls, includeTag, relative bool
	band = v2
	for len(body) > 0 {
		line, length, rest := unpackLine(body)
		if length < 0 {
			return packet(nil, "ERR invalid packet\n")
		}
		body = rest
		if length < 4 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		arg := ""
		if len(fields) > 1 {
			arg = fields[1]
		}
		switch fields[0] {
		case "command=ls-refs":
			ls = true
		case "ref-prefix":
			prefixes = append(prefixes, arg)
		case "peel":
			peel = true
		case "symrefs":
			symrefs = true
		case "unborn":
			unborn = true
		case "want":
			wants = append(wants, arg)
			if len(fields) > 2 {
				band = band || slices.Contains(fields[2:], "side-band-64k")
				ofs = ofs || slices.Contains(fields[2:], "ofs-delta")
				includeTag = includeTag || slices.Contains(fields[2:], "include-tag")
				relative = relative || slices.Contains(fields[2:], "deepen-relative")
			}
		case "include-tag":
			includeTag = true
		case "have":
			if repo.objects[arg] != nil {
				haves = append(haves, arg)
			}
		case "shallow":
			shallow[arg] = true
		case "deepen":
			depth, _ = strconv.Atoi(arg)
		case "deepen-relative":
			relative = true
		case "done":
			done = true
		case "ofs-delta":
			ofs = true
		}
	}
	if ls {
		return repo.lsRefs(prefixes, peel, symrefs, unborn)
	}
	if len(wants) == 0 {
		return []byte("0000")
	}
	for _, oid := range wants {
		if repo.objects[oid] == nil {
			return append(packet(nil, "ERR not our ref "+oid+"\n"), "0000"...)
		}
	}
	var response []byte
	if !done && v2 {
		response = packet(response, "acknowledgments\n")
		if len(haves) == 0 {
			response = packet(response, "NAK\n")
		} else {
			for _, oid := range haves {
				response = packet(response, "ACK "+oid+"\n")
			}
		}
		return append(response, "0000"...)
	}
	if !done && !v2 && len(haves) > 0 {
		return packet(nil, "NAK\n")
	}
	exclude := map[string]bool{}
	var mark func(string)
	mark = func(oid string) {
		if exclude[oid] {
			return
		}
		obj := repo.objects[oid]
		if obj == nil {
			return
		}
		exclude[oid] = true
		switch obj.kind {
		case 1:
			mark(obj.target)
			if !shallow[oid] {
				for _, parent := range obj.parents {
					mark(parent)
				}
			}
		case 2:
			for _, entry := range obj.entries {
				if entry.mode != "160000" {
					mark(entry.oid)
				}
			}
		case 4:
			mark(obj.target)
		}
	}
	for _, oid := range haves {
		mark(oid)
	}
	seen := map[string]bool{}
	boundaries := map[string]bool{}
	extended := map[string]bool{}
	visited := map[string]int{}
	var list []*object
	var visit func(string, int)
	visit = func(oid string, distance int) {
		obj := repo.objects[oid]
		if obj == nil {
			return
		}
		if obj.kind != 1 {
			if seen[oid] || exclude[oid] {
				return
			}
			seen[oid] = true
			list = append(list, obj)
			if obj.kind == 2 {
				for _, entry := range obj.entries {
					if entry.mode != "160000" {
						visit(entry.oid, distance)
					}
				}
			} else if obj.kind == 4 {
				visit(obj.target, distance)
			}
			return
		}
		if previous, ok := visited[oid]; ok && previous <= distance {
			return
		}
		visited[oid] = distance
		if !seen[oid] && !exclude[oid] {
			seen[oid] = true
			list = append(list, obj)
			visit(obj.target, distance)
		}
		if depth <= 0 && shallow[oid] {
			return
		}
		if exclude[oid] && !shallow[oid] {
			return
		}
		if relative && shallow[oid] {
			distance = 0
		}
		if len(obj.parents) > 0 && depth > 0 && distance >= depth {
			if !shallow[oid] && !extended[oid] {
				boundaries[oid] = true
			}
			return
		}
		if shallow[oid] {
			extended[oid] = true
		}
		delete(boundaries, oid)
		for _, parent := range obj.parents {
			visit(parent, distance+1)
		}
	}
	for _, oid := range wants {
		visit(oid, 1)
	}
	if includeTag {
		for _, name := range refNames(repo.refs) {
			oid := repo.refs[name]
			if peeled := repo.objects.peel(oid); peeled != oid && seen[peeled] {
				visit(oid, 1)
			}
		}
	}
	if len(boundaries)+len(extended) > 0 || (!v2 && depth > 0) {
		if v2 {
			response = packet(response, "shallow-info\n")
		}
		for _, oid := range slices.Sorted(mapKeys(boundaries)) {
			response = packet(response, "shallow "+oid+"\n")
		}
		for _, oid := range slices.Sorted(mapKeys(extended)) {
			response = packet(response, "unshallow "+oid+"\n")
		}
		if v2 {
			response = append(response, "0001"...)
		} else {
			response = append(response, "0000"...)
		}
	}
	if v2 {
		response = packet(response, "packfile\n")
	} else if len(haves) > 0 {
		response = packet(response, "ACK "+haves[len(haves)-1]+"\n")
	} else {
		response = packet(response, "NAK\n")
	}
	pack := writePack(list, ofs)
	if band {
		response = append(sideband(response, pack), "0000"...)
	} else {
		response = append(response, pack...)
	}
	return response
}

func mapKeys[Value any](values map[string]Value) func(func(string) bool) {
	return func(yield func(string) bool) {
		for key := range values {
			if !yield(key) {
				return
			}
		}
	}
}

func (repo *repository) lsRefs(prefixes []string, peel, symrefs, unborn bool) []byte {
	var response []byte
	add := func(name, oid string) {
		if len(prefixes) > 0 && !slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(name, prefix) }) {
			return
		}
		line := oid + " " + name
		if name == "HEAD" && (symrefs || oid == "unborn") {
			line += " symref-target:" + repo.head()
		}
		if peel && oid != "unborn" {
			if peeled := repo.objects.peel(oid); peeled != oid {
				line += " peeled:" + peeled
			}
		}
		response = packet(response, line+"\n")
	}
	if head := repo.refs[repo.head()]; head != "" {
		add("HEAD", head)
	} else if unborn {
		add("HEAD", "unborn")
	}
	for _, name := range refNames(repo.refs) {
		add(name, repo.refs[name])
	}
	return append(response, "0000"...)
}
