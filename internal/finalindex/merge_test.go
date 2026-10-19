package finalindex

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Fixtures encode exactly the Rust IRN1 layout, including multi-byte varints.
type fixture struct {
	term  string
	posts []Posting
}

func writeRun(t *testing.T, path string, records []fixture) {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("IRN1")
	binary.Write(&b, binary.LittleEndian, uint64(len(records)))
	for _, rec := range records {
		var docs, freqs []byte
		var prev uint32
		for _, p := range rec.posts {
			docs = binary.AppendUvarint(docs, uint64(p.DocID-prev))
			freqs = binary.AppendUvarint(freqs, uint64(p.Frequency))
			prev = p.DocID
		}
		b.Write(binary.AppendUvarint(nil, uint64(len(rec.term))))
		b.WriteString(rec.term)
		for _, n := range []uint64{uint64(len(rec.posts)), uint64(prev), uint64(len(docs)), uint64(len(freqs))} {
			b.Write(binary.AppendUvarint(nil, n))
		}
		b.Write(docs)
		b.Write(freqs)
	}
	if err := os.WriteFile(path, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}
func fixtureInput(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "runs"), 0755)
	writeRun(t, filepath.Join(dir, "runs/postings-000000.bin"), []fixture{{"apple", []Posting{{0, 2}}}, {"cat", []Posting{{0, 1}, {127, 300}}}})
	writeRun(t, filepath.Join(dir, "runs/postings-000001.bin"), []fixture{{"cat", []Posting{{128, 2}, {300, 4}}}, {"zebra", []Posting{{299, 1}}}})
	pages := make([]byte, 301*16)
	for i := 0; i < 301; i++ {
		binary.LittleEndian.PutUint32(pages[i*16:], uint32(1000+i))
		binary.LittleEndian.PutUint32(pages[i*16+4:], uint32(i+1))
		binary.LittleEndian.PutUint64(pages[i*16+8:], uint64(i*100))
	}
	os.WriteFile(filepath.Join(dir, "pagetable.bin"), pages, 0600)
	os.WriteFile(filepath.Join(dir, "meta.txt"), []byte("num_docs=301\nnum_postings=6\nnum_runs=2\n"), 0600)
	return dir
}
func TestMergeAndSeek(t *testing.T) {
	in := fixtureInput(t)
	out := filepath.Join(t.TempDir(), "index")
	stats, err := Merge(in, out, 2)
	if err != nil {
		t.Fatal(err)
	}
	if stats != (Stats{3, 6, 4}) {
		t.Fatalf("stats %+v", stats)
	}
	r, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	raw, _ := os.ReadFile(filepath.Join(out, "lexicon.tsv"))
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Scan()
	entries := map[string]Entry{}
	for sc.Scan() {
		e, err := ParseEntry(sc.Text())
		if err != nil {
			t.Fatal(err)
		}
		entries[e.Term] = e
	}
	descriptors, err := r.BlockDescriptors(entries["cat"])
	if err != nil {
		t.Fatal(err)
	}
	want_descriptors := []BlockDescriptor{
		{LastDocID: 127, PostingCount: 2, PayloadOffset: 6, DocumentBytes: 2, FrequencyBytes: 3},
		{LastDocID: 300, PostingCount: 2, PayloadOffset: 11, DocumentBytes: 4, FrequencyBytes: 2},
	}
	if !reflect.DeepEqual(descriptors, want_descriptors) {
		t.Fatalf("descriptors: got %+v, want %+v", descriptors, want_descriptors)
	}
	want_blocks := [][]Posting{{{0, 1}, {127, 300}}, {{128, 2}, {300, 4}}}
	for index, descriptor := range descriptors {
		postings, err := r.ReadPostingBlock(descriptor)
		if err != nil || !reflect.DeepEqual(postings, want_blocks[index]) {
			t.Fatalf("block %d: got %+v, error %v", index, postings, err)
		}
	}
	invalid_descriptor := descriptors[0]
	invalid_descriptor.PostingCount = 0
	if _, err := r.ReadPostingBlock(invalid_descriptor); err == nil {
		t.Fatal("expected invalid posting block error")
	}
	invalid_entry := entries["cat"]
	invalid_entry.BlockCount = ^uint64(0)
	if _, err := r.BlockDescriptors(invalid_entry); err == nil {
		t.Fatal("expected invalid descriptor range error")
	}
	invalid_entry = entries["cat"]
	invalid_entry.DocFrequency++
	if _, err := r.BlockDescriptors(invalid_entry); err == nil {
		t.Fatal("expected descriptor count mismatch error")
	}
	for _, tc := range []struct {
		target uint32
		want   Posting
		ok     bool
	}{{0, Posting{0, 1}, true}, {1, Posting{127, 300}, true}, {127, Posting{127, 300}, true}, {128, Posting{128, 2}, true}, {129, Posting{300, 4}, true}, {301, Posting{}, false}} {
		p, ok, err := r.SeekGE(entries["cat"], tc.target)
		if err != nil || ok != tc.ok || p != tc.want {
			t.Fatalf("target %d: %+v %v %v", tc.target, p, ok, err)
		}
	}
	d, err := r.Document(128)
	if err != nil || d != (Document{1128, 129, 12800}) {
		t.Fatalf("document %+v %v", d, err)
	}
	if _, err = r.Document(301); err == nil {
		t.Fatal("expected missing doc error")
	}
	if _, err = Merge(in, out, 2); err == nil {
		t.Fatal("must refuse existing output")
	}
}
func TestRejectCorruptRun(t *testing.T) {
	for _, kind := range []string{"trailing", "truncated", "overlap"} {
		t.Run(kind, func(t *testing.T) {
			in := fixtureInput(t)
			p := filepath.Join(in, "runs/postings-000001.bin")
			b, _ := os.ReadFile(p)
			switch kind {
			case "trailing":
				b = append(b, 0)
			case "truncated":
				b = b[:len(b)-1]
			case "overlap":
				writeRun(t, p, []fixture{{"cat", []Posting{{127, 1}}}})
				b, _ = os.ReadFile(p)
			}
			os.WriteFile(p, b, 0600)
			out := filepath.Join(t.TempDir(), "index")
			if _, err := Merge(in, out, 2); err == nil {
				t.Fatal("accepted corrupt input")
			}
			if _, err := Open(out); err == nil {
				t.Fatal("opened failed index")
			}
		})
	}
}
func TestMalformedVarint(t *testing.T) {
	in := fixtureInput(t)
	p := filepath.Join(in, "runs/postings-000000.bin")
	b, _ := os.ReadFile(p)
	b = append(b[:12], bytes.Repeat([]byte{0xff}, 11)...)
	os.WriteFile(p, b, 0600)
	if _, err := Merge(in, filepath.Join(t.TempDir(), "index"), 128); err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("error %v", err)
	}
}

func TestOpenMissingFiles(t *testing.T) {
	if _, err := Open(t.TempDir()); err == nil {
		t.Fatal("expected missing file error")
	}
}

// This checks actual Rust-produced bytes against a fully decoded reference.
// It runs when the release parser and collection are available locally.
func TestRustInterop(t *testing.T) {
	parser, err := filepath.Abs("../../target/release/parse")
	if err != nil {
		t.Fatal(err)
	}
	collection, err := filepath.Abs("../../data/collection.tsv")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{parser, collection} {
		if _, err := os.Stat(p); err != nil {
			t.Skip("Rust release parser and collection required")
		}
	}
	input_paths := filepath.Join(t.TempDir(), "rust")
	command := exec.Command(parser, "--input", collection, "--dir", input_paths, "--limit", "20000", "--mem-mb", "4")
	if b, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Rust parser: %v\n%s", err, b)
	}
	paths, _ := filepath.Glob(filepath.Join(input_paths, "runs", "*.bin"))
	if len(paths) < 2 {
		t.Fatal("expected multiple Rust runs")
	}
	reference := map[string][]Posting{}
	for i, p := range paths {
		rr, err := openRun(p, i)
		if err != nil {
			t.Fatal(err)
		}
		for {
			ok, err := rr.next()
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			dr, fr := bytes.NewReader(rr.docs), bytes.NewReader(rr.freqs)
			var doc uint64
			for j := uint64(0); j < rr.count; j++ {
				gap, err := binary.ReadUvarint(dr)
				if err != nil {
					t.Fatal(err)
				}
				freq, err := binary.ReadUvarint(fr)
				if err != nil {
					t.Fatal(err)
				}
				doc += gap
				reference[rr.term] = append(reference[rr.term], Posting{uint32(doc), uint32(freq)})
			}
		}
		rr.file.Close()
	}
	output_paths := filepath.Join(t.TempDir(), "index")
	stats, err := Merge(input_paths, output_paths, 128)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Terms != uint64(len(reference)) {
		t.Fatal("vocabulary mismatch")
	}
	r, err := Open(output_paths)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	f, err := os.Open(filepath.Join(output_paths, "lexicon.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan()
	for sc.Scan() {
		e, err := ParseEntry(sc.Text())
		if err != nil {
			t.Fatal(err)
		}
		want := reference[e.Term]
		if e.DocFrequency != uint64(len(want)) {
			t.Fatalf("frequency mismatch: %s", e.Term)
		}
		// Fully decode every output block, comparing every posting to its Rust source.
		var got []Posting
		for i := uint64(0); i < e.BlockCount; i++ {
			b, err := r.block(e, i)
			if err != nil {
				t.Fatal(err)
			}
			payload := make([]byte, int(b.docs)+int(b.freqs))
			if _, err := r.posts.ReadAt(payload, int64(b.offset)); err != nil {
				t.Fatal(err)
			}
			dr, fr := bytes.NewReader(payload[:b.docs]), bytes.NewReader(payload[b.docs:])
			var doc uint64
			for j := uint32(0); j < b.count; j++ {
				g, err := binary.ReadUvarint(dr)
				if err != nil {
					t.Fatal(err)
				}
				freq, err := binary.ReadUvarint(fr)
				if err != nil {
					t.Fatal(err)
				}
				doc += g
				got = append(got, Posting{uint32(doc), uint32(freq)})
			}
			if doc != uint64(b.last) || dr.Len() != 0 || fr.Len() != 0 {
				t.Fatal("invalid output block")
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("posting mismatch: %s", e.Term)
		}
		// Seek into each nonempty term, including an absent docID where possible.
		target := want[len(want)/2].DocID
		p, ok, err := r.SeekGE(e, target)
		if err != nil || !ok || p != want[len(want)/2] {
			t.Fatalf("seek mismatch %s: %v", e.Term, err)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	source, _ := os.ReadFile(filepath.Join(input_paths, "pagetable.bin"))
	copied, _ := os.ReadFile(filepath.Join(output_paths, "pagetable.bin"))
	if !bytes.Equal(source, copied) {
		t.Fatal("page table changed")
	}
}
