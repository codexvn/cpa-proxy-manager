package main

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"cpa-proxy-manager/cpasdk/pluginapi"
)

func TestMatchFilter(t *testing.T) {
	cases := []struct {
		name     string
		entry    pluginapi.HostAuthFileEntry
		q        string
		provider string
		want     bool
	}{
		{"empty filter matches all", pluginapi.HostAuthFileEntry{Name: "a.json"}, "", "", true},
		{"provider exact match", pluginapi.HostAuthFileEntry{Name: "a.json", Provider: "claude"}, "", "claude", true},
		{"provider mismatch", pluginapi.HostAuthFileEntry{Name: "a.json", Provider: "claude"}, "", "gemini", false},
		{"q matches email case-insensitively", pluginapi.HostAuthFileEntry{Name: "a.json", Email: "User@Example.com"}, "user@", "", true},
		{"q matches name", pluginapi.HostAuthFileEntry{Name: "team-a.json"}, "team-a", "", true},
		{"q matches nothing", pluginapi.HostAuthFileEntry{Name: "a.json", Email: "u@e.com"}, "zzz", "", false},
		{"empty email never matches q", pluginapi.HostAuthFileEntry{Name: "a.json"}, "x", "", false},
		{"q and provider both apply", pluginapi.HostAuthFileEntry{Name: "a.json", Provider: "claude", Email: "u@e.com"}, "u@", "gemini", false},
	}
	for _, tc := range cases {
		if got := MatchFilter(tc.entry, tc.q, tc.provider); got != tc.want {
			t.Errorf("%s: MatchFilter(%q, %q) = %v, want %v", tc.name, tc.q, tc.provider, got, tc.want)
		}
	}
}

func TestFilterEntriesDropsRuntimeOnly(t *testing.T) {
	entries := []pluginapi.HostAuthFileEntry{
		{AuthIndex: "i1", Name: "a.json"},
		{AuthIndex: "", Name: "no-index.json"},
		{AuthIndex: "i2", Name: "b.json", RuntimeOnly: true},
	}
	got := FilterEntries(entries, "", "", "")
	if len(got) != 1 || got[0].AuthIndex != "i1" {
		t.Fatalf("FilterEntries kept %v, want only i1", got)
	}
}

func TestParsePageFilter(t *testing.T) {
	f := ParsePageFilter(url.Values{})
	if f.Page != 1 || f.Size != defaultPageSize {
		t.Fatalf("defaults: page=%d size=%d, want 1/%d", f.Page, f.Size, defaultPageSize)
	}
	f = ParsePageFilter(url.Values{"page": {"3"}, "size": {"100"}, "q": {" alice "}, "provider": {"claude"}, "bound": {" bound "}})
	if f.Page != 3 || f.Size != 100 || f.Q != "alice" || f.Provider != "claude" || f.Bound != "bound" {
		t.Fatalf("parsed: %+v", f)
	}
	f = ParsePageFilter(url.Values{"page": {"0"}, "size": {"9999"}, "bound": {"nope"}})
	if f.Page != 1 || f.Size != maxPageSize || f.Bound != "" {
		t.Fatalf("clamped: page=%d size=%d bound=%q, want 1/%d empty", f.Page, f.Size, f.Bound, maxPageSize)
	}
}

func TestFilterEntriesByBound(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	entries := []pluginapi.HostAuthFileEntry{
		{AuthIndex: "bound", Name: "bound.json", Provider: "claude", Path: write("bound.json", `{"proxy_url":"http://127.0.0.1:1"}`)},
		{AuthIndex: "empty", Name: "empty.json", Provider: "claude", Path: write("empty.json", `{"proxy_url":""}`)},
		{AuthIndex: "blank", Name: "blank.json", Provider: "claude", Path: write("blank.json", `{"proxy_url":"  "}`)},
		{AuthIndex: "missing-key", Name: "nokey.json", Provider: "claude", Path: write("nokey.json", `{}`)},
		{AuthIndex: "number", Name: "number.json", Provider: "claude", Path: write("number.json", `{"proxy_url":1}`)},
		{AuthIndex: "unread", Name: "unread.json", Provider: "claude", Path: filepath.Join(dir, "no-such.json")},
		{AuthIndex: "runtime", Name: "runtime.json", Provider: "claude", RuntimeOnly: true, Path: write("runtime.json", `{"proxy_url":"http://127.0.0.1:9"}`)},
		{AuthIndex: "gemini", Name: "gemini.json", Provider: "gemini", Path: write("gemini.json", `{"proxy_url":"socks5://127.0.0.1:2"}`)},
	}
	ids := func(got []pluginapi.HostAuthFileEntry) []string {
		out := make([]string, len(got))
		for i, e := range got {
			out[i] = e.AuthIndex
		}
		return out
	}
	if got := ids(FilterEntries(entries, "", "claude", "bound")); len(got) != 1 || got[0] != "bound" {
		t.Fatalf("bound claude: %v", got)
	}
	if got := ids(FilterEntries(entries, "", "claude", "unbound")); len(got) != 4 || got[0] != "empty" || got[1] != "blank" || got[2] != "missing-key" || got[3] != "number" {
		t.Fatalf("unbound claude: %v", got)
	}
	if got := ids(FilterEntries(entries, "", "gemini", "bound")); len(got) != 1 || got[0] != "gemini" {
		t.Fatalf("bound gemini: %v", got)
	}
	if got := ids(FilterEntries(entries, "", "", "")); len(got) != 7 || got[0] != "bound" || got[6] != "gemini" {
		t.Fatalf("no binding filter: %v", got)
	}
}

func TestPageSlice(t *testing.T) {
	items := []int{1, 2, 3, 4, 5}
	got, page, pages := PageSlice(items, 1, 2)
	if len(got) != 2 || got[0] != 1 || page != 1 || pages != 3 {
		t.Fatalf("first page: %v page=%d pages=%d", got, page, pages)
	}
	got, page, pages = PageSlice(items, 99, 2)
	if len(got) != 1 || got[0] != 5 || page != 3 || pages != 3 {
		t.Fatalf("overflow page clamps to last: %v page=%d pages=%d", got, page, pages)
	}
	_, page, pages = PageSlice([]int(nil), 5, 50)
	if page != 1 || pages != 1 {
		t.Fatalf("empty slice: page=%d pages=%d, want 1/1", page, pages)
	}
}
