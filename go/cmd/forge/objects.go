package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const zero = "0000000000000000000000000000000000000000"
const maxObject = 32 << 20

var kinds = [...]string{"", "commit", "tree", "blob", "tag"}
var badPack = errors.New("invalid packfile")

type object struct {
	kind         byte
	data         []byte
	compressed   []byte
	compressOnce sync.Once
	parents      []string
	target       string
	entries      []treeEntry
	message      string
}
type treeEntry struct{ mode, name, oid string }
type objects map[string]*object

func objectID(kind byte, data []byte) string {
	header := strconv.AppendInt(append([]byte(kinds[kind]), ' '), int64(len(data)), 10)
	hash := sha1.New()
	hash.Write(append(header, 0))
	hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil))
}

func newObject(kind byte, data, compressed []byte) *object {
	obj := &object{kind: kind, data: data, compressed: compressed}
	switch kind {
	case 1, 4:
		head, body, _ := strings.Cut(string(data), "\n\n")
		obj.message = strings.TrimSuffix(body, "\n")
		for line := range strings.SplitSeq(head, "\n") {
			key, value, _ := strings.Cut(line, " ")
			switch key {
			case "tree", "object":
				obj.target = value
			case "parent":
				obj.parents = append(obj.parents, value)
			}
		}
	case 2:
		for rest := data; len(rest) > 0; {
			nul := bytes.IndexByte(rest, 0)
			if nul < 0 || len(rest) < nul+21 {
				break
			}
			mode, name, ok := strings.Cut(string(rest[:nul]), " ")
			if !ok {
				break
			}
			obj.entries = append(obj.entries, treeEntry{mode, name, hex.EncodeToString(rest[nul+1 : nul+21])})
			rest = rest[nul+21:]
		}
	}
	return obj
}

func (store objects) put(kind byte, data []byte) string {
	oid := objectID(kind, data)
	if store[oid] == nil {
		store[oid] = newObject(kind, data, nil)
	}
	return oid
}

func (store objects) peel(oid string) string {
	for depth := 0; depth < 64; depth++ {
		obj := store[oid]
		if obj == nil || obj.kind != 4 {
			return oid
		}
		oid = obj.target
	}
	return oid
}

func (store objects) ancestor(old, next string) bool {
	stack := []string{next}
	seen := map[string]bool{}
	for len(stack) > 0 {
		last := len(stack) - 1
		oid := stack[last]
		stack = stack[:last]
		if oid == old {
			return true
		}
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if obj := store[oid]; obj != nil && obj.kind == 1 {
			stack = append(stack, obj.parents...)
		}
	}
	return false
}

func (store objects) flat(tree, prefix string, files map[string]treeEntry) {
	if len(prefix) > 4096 {
		return
	}
	if obj := store[tree]; obj != nil && obj.kind == 2 {
		for _, entry := range obj.entries {
			name := prefix + entry.name
			if entry.mode == "40000" || entry.mode == "040000" {
				store.flat(entry.oid, name+"/", files)
			} else {
				files[name] = entry
			}
		}
	}
}

func (store objects) makeTree(files map[string]treeEntry) string {
	dirs := map[string]map[string]treeEntry{}
	entries := make([]treeEntry, 0, len(files))
	for name, entry := range files {
		dir, rest, found := strings.Cut(name, "/")
		if !found {
			entry.name = name
			entries = append(entries, entry)
			continue
		}
		if dirs[dir] == nil {
			dirs[dir] = map[string]treeEntry{}
		}
		dirs[dir][rest] = entry
	}
	for dir, children := range dirs {
		entries = append(entries, treeEntry{"40000", dir, store.makeTree(children)})
	}
	sort.Slice(entries, func(left, right int) bool {
		a, b := entries[left].name, entries[right].name
		if entries[left].mode == "40000" {
			a += "/"
		}
		if entries[right].mode == "40000" {
			b += "/"
		}
		return a < b
	})
	var data []byte
	for _, entry := range entries {
		data = append(data, entry.mode+" "+entry.name+"\x00"...)
		data, _ = hex.AppendDecode(data, []byte(entry.oid))
	}
	return store.put(2, data)
}

func deltaInt(data []byte, offset *int) (int, bool) {
	value := 0
	for shift := 0; shift < 35 && *offset < len(data); shift += 7 {
		part := data[*offset]
		*offset++
		value |= int(part&127) << shift
		if part < 128 {
			return value, true
		}
	}
	return 0, false
}

func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	size, ok := deltaInt(delta, &pos)
	if !ok || size != len(base) {
		return nil, badPack
	}
	size, ok = deltaInt(delta, &pos)
	if !ok || size > maxObject {
		return nil, badPack
	}
	result := make([]byte, 0, size)
	for pos < len(delta) {
		code := delta[pos]
		pos++
		if code&128 == 0 {
			count := int(code)
			if count == 0 || pos+count > len(delta) || len(result)+count > size {
				return nil, badPack
			}
			result = append(result, delta[pos:pos+count]...)
			pos += count
			continue
		}
		start, count := 0, 0
		for bit := 0; bit < 7; bit++ {
			if code&(1<<bit) != 0 {
				if pos >= len(delta) {
					return nil, badPack
				}
				part := int(delta[pos])
				pos++
				if bit < 4 {
					start |= part << (bit * 8)
				} else {
					count |= part << ((bit - 4) * 8)
				}
			}
		}
		if count == 0 {
			count = 65536
		}
		if start+count > len(base) || len(result)+count > size {
			return nil, badPack
		}
		result = append(result, base[start:start+count]...)
	}
	if len(result) != size {
		return nil, badPack
	}
	return result, nil
}

type packEntry struct {
	offset, baseOffset int
	baseID             string
	data               []byte
	obj                *object
}

var readers sync.Pool
var compressors = sync.Pool{New: func() any { writer, _ := zlib.NewWriterLevel(io.Discard, zlib.BestSpeed); return writer }}

func readPack(raw []byte, existing objects) (objects, error) {
	return readPackLookup(raw, func(oid string) *object { return existing[oid] })
}

func readPackLookup(raw []byte, existing func(string) *object) (objects, error) {
	if len(raw) < 32 || string(raw[:4]) != "PACK" {
		return nil, badPack
	}
	version := binary.BigEndian.Uint32(raw[4:])
	if version != 2 && version != 3 {
		return nil, badPack
	}
	end := len(raw) - 20
	checksum := sha1.Sum(raw[:end])
	if !bytes.Equal(checksum[:], raw[end:]) {
		return nil, badPack
	}
	count := int(binary.BigEndian.Uint32(raw[8:]))
	if count > (end-12)/9 {
		return nil, badPack
	}
	entries := make([]packEntry, count)
	offsets := make(map[int]*packEntry, count)
	output := make(objects, count)
	reader := bytes.NewReader(nil)
	pos := 12
	total := 0
	for index := range entries {
		if pos >= end {
			return nil, badPack
		}
		entry := &entries[index]
		entry.offset = pos
		offsets[pos] = entry
		code := raw[pos]
		pos++
		kind := (code >> 4) & 7
		size := int(code & 15)
		for shift := 4; code&128 != 0; shift += 7 {
			if pos >= end || shift > 32 {
				return nil, badPack
			}
			code = raw[pos]
			pos++
			size |= int(code&127) << shift
		}
		total += size
		if size > maxObject || total > 128<<20 {
			return nil, badPack
		}
		switch kind {
		case 1, 2, 3, 4:
		case 6:
			if pos >= end {
				return nil, badPack
			}
			part := raw[pos]
			pos++
			distance := int(part & 127)
			for part&128 != 0 {
				if pos >= end || distance > entry.offset {
					return nil, badPack
				}
				part = raw[pos]
				pos++
				distance = ((distance + 1) << 7) | int(part&127)
			}
			entry.baseOffset = entry.offset - distance
			if offsets[entry.baseOffset] == nil || distance == 0 {
				return nil, badPack
			}
		case 7:
			if end-pos < 20 {
				return nil, badPack
			}
			entry.baseID = hex.EncodeToString(raw[pos : pos+20])
			pos += 20
		default:
			return nil, badPack
		}
		start := pos
		reader.Reset(raw[pos:end])
		var inflater io.ReadCloser
		var err error
		if pooled := readers.Get(); pooled != nil {
			inflater = pooled.(io.ReadCloser)
			err = inflater.(zlib.Resetter).Reset(reader, nil)
		} else {
			inflater, err = zlib.NewReader(reader)
		}
		if err != nil {
			return nil, badPack
		}
		data := make([]byte, size)
		_, err = io.ReadFull(inflater, data)
		if err == nil {
			var extra [1]byte
			amount, last := inflater.Read(extra[:])
			if amount != 0 || last != io.EOF {
				err = badPack
			}
		}
		inflater.Close()
		readers.Put(inflater)
		if err != nil {
			return nil, badPack
		}
		pos = end - reader.Len()
		if kind <= 4 {
			entry.obj = newObject(kind, data, bytes.Clone(raw[start:pos]))
			output[objectID(kind, data)] = entry.obj
		} else {
			entry.data = data
		}
	}
	if pos != end {
		return nil, badPack
	}
	for {
		pending, progress := 0, false
		for index := range entries {
			entry := &entries[index]
			if entry.obj != nil {
				continue
			}
			var base *object
			if entry.baseID != "" {
				base = output[entry.baseID]
				if base == nil {
					base = existing(entry.baseID)
				}
			} else {
				base = offsets[entry.baseOffset].obj
			}
			if base == nil {
				pending++
				continue
			}
			data, err := applyDelta(base.data, entry.data)
			if err != nil {
				return nil, err
			}
			total += len(data)
			if total > 128<<20 {
				return nil, badPack
			}
			entry.obj = newObject(base.kind, data, nil)
			output[objectID(base.kind, data)] = entry.obj
			progress = true
		}
		if pending == 0 {
			return output, nil
		}
		if !progress {
			return nil, badPack
		}
	}
}

func packHeader(kind byte, size int) []byte {
	code := kind<<4 | byte(size&15)
	size >>= 4
	var out []byte
	for size > 0 {
		out = append(out, code|128)
		code = byte(size & 127)
		size >>= 7
	}
	return append(out, code)
}

func deflate(data []byte) []byte {
	var buf bytes.Buffer
	writer := compressors.Get().(*zlib.Writer)
	writer.Reset(&buf)
	writer.Write(data)
	writer.Close()
	writer.Reset(io.Discard)
	compressors.Put(writer)
	return buf.Bytes()
}

func offsetBytes(distance int) []byte {
	var storage [10]byte
	index := len(storage) - 1
	storage[index] = byte(distance & 127)
	for distance>>7 > 0 {
		distance = (distance >> 7) - 1
		index--
		storage[index] = 128 | byte(distance&127)
	}
	return storage[index:]
}

func simpleDelta(base, data []byte) []byte {
	prefix := 0
	for prefix < len(base) && prefix < len(data) && prefix < 0xffffff && base[prefix] == data[prefix] {
		prefix++
	}
	if prefix < 32 {
		return nil
	}
	out := binary.AppendUvarint(nil, uint64(len(base)))
	out = binary.AppendUvarint(out, uint64(len(data)))
	code := byte(128)
	var args []byte
	for index := 0; index < 3; index++ {
		if value := byte(prefix >> (index * 8)); value != 0 {
			code |= 1 << (index + 4)
			args = append(args, value)
		}
	}
	out = append(out, code)
	out = append(out, args...)
	for rest := data[prefix:]; len(rest) > 0; {
		length := min(127, len(rest))
		out = append(out, byte(length))
		out = append(out, rest[:length]...)
		rest = rest[length:]
	}
	return out
}

func writePack(list []*object, deltas bool) []byte {
	out := make([]byte, 0, 32+len(list)*128)
	out = append(out, "PACK\x00\x00\x00\x02"...)
	out = binary.BigEndian.AppendUint32(out, uint32(len(list)))
	var previous [5]*object
	var offsets [5]int
	for _, obj := range list {
		start := len(out)
		obj.compressOnce.Do(func() {
			if obj.compressed == nil {
				obj.compressed = deflate(obj.data)
			}
		})
		compressed := obj.compressed
		header := packHeader(obj.kind, len(obj.data))
		var offset []byte
		if base := previous[obj.kind]; deltas && base != nil && len(obj.data) < 1<<20 {
			if delta := simpleDelta(base.data, obj.data); delta != nil {
				trial := deflate(delta)
				distance := offsetBytes(start - offsets[obj.kind])
				deltaHeader := packHeader(6, len(delta))
				if len(trial)+len(distance)+len(deltaHeader) < len(compressed)+len(header) {
					header, compressed, offset = deltaHeader, trial, distance
				}
			}
		}
		out = append(out, header...)
		out = append(out, offset...)
		out = append(out, compressed...)
		previous[obj.kind] = obj
		offsets[obj.kind] = start
	}
	checksum := sha1.Sum(out)
	return append(out, checksum[:]...)
}
