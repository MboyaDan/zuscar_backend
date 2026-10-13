package migrate

import (
	"strings"
	"testing"
	"testing/fstest"
)

func fsOf(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for k, v := range files {
		m["m/"+k] = &fstest.MapFile{Data: []byte(v)}
	}
	return m
}

func TestLoadOrdersAndChecksums(t *testing.T) {
	migs, err := Load(fsOf(map[string]string{"0002_b.sql": "select 2;", "0001_a.sql": "select 1;"}), "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(migs) != 2 || migs[0].Name != "a" || migs[1].Name != "b" || len(migs[0].Checksum) != 64 || migs[0].Checksum == migs[1].Checksum {
		t.Fatalf("unexpected: %+v", migs)
	}
}

func TestLoadRejectsBadNamesAndGaps(t *testing.T) {
	if _, err := Load(fsOf(map[string]string{"1_x.sql": "x"}), "m"); err == nil || !strings.Contains(err.Error(), "NNNN_description.sql") {
		t.Fatalf("bad name accepted: %v", err)
	}
	if _, err := Load(fsOf(map[string]string{"0001_a.sql": "x", "0003_c.sql": "x"}), "m"); err == nil || !strings.Contains(err.Error(), "gaps") {
		t.Fatalf("gap accepted: %v", err)
	}
	if _, err := Load(fsOf(map[string]string{"0001_a.sql": "x", "0001_b.sql": "x"}), "m"); err == nil {
		t.Fatal("duplicate version accepted")
	}
}
