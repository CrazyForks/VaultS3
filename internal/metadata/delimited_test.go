package metadata

import (
	"fmt"
	"testing"
)

func TestListLatestObjectsDelimited(t *testing.T) {
	s := newTestStore(t)
	put := func(key string) {
		if err := s.PutObjectMeta(ObjectMeta{Bucket: "b", Key: key, Size: 1, ContentType: "text/plain", LastModified: 1700000000}); err != nil {
			t.Fatal(err)
		}
	}
	put("a.txt")
	put("z.txt")
	for i := 0; i < 50; i++ { // a folder with many objects
		put(fmt.Sprintf("folderA/obj%03d.dat", i))
	}
	put("folderB/only.dat")

	// Root level: folders collapse to ONE prefix each (folderA's 50 objects are
	// skipped past, not listed), direct objects appear individually.
	objs, prefixes, trunc, _, err := s.ListLatestObjectsDelimited("b", "", "/", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(prefixes) != 2 || prefixes[0].Prefix != "folderA/" || prefixes[1].Prefix != "folderB/" {
		t.Fatalf("prefixes = %v, want [folderA/ folderB/]", prefixes)
	}
	// Each folder carries a date sourced from the first key under it (issue #35).
	if prefixes[0].LastModified == 0 || prefixes[1].LastModified == 0 {
		t.Fatalf("folders should carry a LastModified, got %+v", prefixes)
	}
	if len(objs) != 2 || objs[0].Key != "a.txt" || objs[1].Key != "z.txt" {
		t.Fatalf("objects = %v, want a.txt, z.txt", keysOf(objs))
	}
	if trunc {
		t.Fatal("should not be truncated")
	}

	// Drill into the heavy folder: all 50 objects, no sub-prefixes.
	objs, prefixes, _, _, _ = s.ListLatestObjectsDelimited("b", "folderA/", "/", "", 1000)
	if len(objs) != 50 || len(prefixes) != 0 {
		t.Fatalf("drill-down: got %d objects, %d prefixes, want 50/0", len(objs), len(prefixes))
	}

	// Cursor pagination over the 4 root entries (2 folders + 2 objects), 2 per page.
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		o, p, tr, next, _ := s.ListLatestObjectsDelimited("b", "", "/", cursor, 2)
		for _, x := range o {
			seen[x.Key] = true
		}
		for _, x := range p {
			seen[x.Prefix] = true
		}
		pages++
		if !tr {
			break
		}
		cursor = next
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	for _, want := range []string{"a.txt", "z.txt", "folderA/", "folderB/"} {
		if !seen[want] {
			t.Fatalf("pagination missed %q (saw %v)", want, seen)
		}
	}

	// No delimiter → flat listing of every key.
	flat, _, _, _, _ := s.ListLatestObjectsDelimited("b", "", "", "", 1000)
	if len(flat) != 53 { // a.txt, z.txt, 50 folderA, folderB
		t.Fatalf("flat listing = %d keys, want 53", len(flat))
	}
}

func keysOf(m []ObjectMeta) []string {
	out := make([]string, len(m))
	for i, x := range m {
		out[i] = x.Key
	}
	return out
}
