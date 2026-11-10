package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	spec := "../../examples/registry/strategies/EmaCross.json"
	var out, errw bytes.Buffer
	if code := run([]string{"gen", spec}, &out, &errw); code != 0 {
		t.Fatalf("gen %d %s", code, errw.String())
	}
	want, _ := os.ReadFile("../../examples/registry/strategies/EmaCross.py")
	if out.String() != string(want) {
		t.Fatal("gen output differs from the golden file")
	}
	out.Reset()
	if run([]string{"describe", spec}, &out, &errw) != 0 || !strings.HasPrefix(out.String(), "EmaCross trades 1d") {
		t.Fatalf("describe %q", out.String())
	}
	bad := t.TempDir() + "/bad.json"
	os.WriteFile(bad, []byte(`{"version":1,"name":"x"}`), 0o644)
	if run([]string{"check", bad}, &out, &errw) != 1 {
		t.Fatal("an invalid spec must exit 1")
	}
	if run([]string{"nope", spec}, &out, &errw) != 2 || run(nil, &out, &errw) != 2 {
		t.Fatal("usage errors exit 2")
	}
}
