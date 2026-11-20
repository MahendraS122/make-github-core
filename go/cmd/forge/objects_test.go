package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"testing"
)

func TestPackDeltas(t *testing.T) {
	base := bytes.Repeat([]byte("abcdefghij"), 300)
	next := append(bytes.Clone(base), []byte(" changed")...)
	last := append(bytes.Clone(next), []byte(" again")...)
	list := []*object{newObject(3, base, nil), newObject(3, next, nil), newObject(3, last, nil)}
	pack := writePack(list, true)
	if len(pack) >= len(writePack(list, false)) {
		t.Fatal("expected OFS deltas")
	}
	got, err := readPack(pack, nil)
	if err != nil || len(got) != 3 || !bytes.Equal(got[objectID(3, last)].data, last) {
		t.Fatalf("OFS chain: %v", err)
	}
	ref := []byte("PACK\x00\x00\x00\x02\x00\x00\x00\x02")
	for _, pair := range [][2][]byte{{next, last}, {base, next}} {
		delta := simpleDelta(pair[0], pair[1])
		ref = append(ref, packHeader(7, len(delta))...)
		ref, _ = hex.AppendDecode(ref, []byte(objectID(3, pair[0])))
		ref = append(ref, deflate(delta)...)
	}
	sum := sha1.Sum(ref)
	ref = append(ref, sum[:]...)
	got, err = readPack(ref, objects{objectID(3, base): newObject(3, base, nil)})
	if err != nil || len(got) != 2 || !bytes.Equal(got[objectID(3, last)].data, last) {
		t.Fatalf("REF chain: %v", err)
	}
	ref[12] ^= 1
	if _, err = readPack(ref, nil); err == nil {
		t.Fatal("accepted corrupted pack")
	}
}
