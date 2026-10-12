package copier

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJournalSurvivesCutOffLine(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir, "b")
	if err != nil {
		t.Fatal(err)
	}
	a := Entry{Key: "a", SrcVersion: "1", Size: 1, State: "done", DstVersion: "x"}
	if err := j.Record(a); err != nil {
		t.Fatal(err)
	}
	j.Close()
	path := filepath.Join(dir, "b.jsonl")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"key":"b","src_ver`) // a crash mid-write
	f.Close()

	for run := 2; run <= 3; run++ {
		j, err := OpenJournal(dir, "b")
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if _, ok := j.Done(a); !ok {
			t.Fatalf("run %d lost the finished entry", run)
		}
		if err := j.Record(Entry{Key: "c" + string(rune('0'+run)), State: "done", DstVersion: "y"}); err != nil {
			t.Fatal(err)
		}
		j.Close()
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), `"src_ver`+"\n") || strings.Count(string(data), "\n") != 3 {
		t.Fatalf("journal not repaired:\n%s", data)
	}
}

func TestJournalReportsDamageInTheMiddle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "b.jsonl")
	os.WriteFile(path, []byte("{\"key\":\"a\",\"state\":\"done\"}\nnot json\n{\"key\":\"b\",\"state\":\"done\"}\n"), 0o600)
	if _, err := OpenJournal(dir, "b"); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("expected damage at line 2, got %v", err)
	}
}

func TestKnownDstKeepsEarlierVersions(t *testing.T) {
	j, err := OpenJournal(t.TempDir(), "b")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	j.Record(Entry{Key: "k", SrcVersion: "1", State: "done", DstVersion: "d1"})
	if !j.KnownDst("k", "d1") || j.KnownDst("k", "d2") || j.KnownDst("other", "d1") {
		t.Fatal("KnownDst is wrong")
	}
}
