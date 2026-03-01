package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test infrastructure
// ---------------------------------------------------------------------------

func TestMain(m *testing.M) {
	var err error
	db, err = NewSQLiteDB(":memory:")
	if err != nil {
		fmt.Fprintf(os.Stderr, "setup: %v\n", err)
		os.Exit(1)
	}
	*maxResolveDepth = 5
	currentUser = func(r *http.Request) (string, bool, error) {
		return "test@example.com", false, nil
	}
	code := m.Run()
	db.db.Close()
	os.Exit(code)
}

func resetDB(t *testing.T) {
	t.Helper()
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, err := db.db.Exec("DELETE FROM Linx"); err != nil {
		t.Fatalf("resetDB Linx: %v", err)
	}
	if _, err := db.db.Exec("DELETE FROM Settings"); err != nil {
		t.Fatalf("resetDB Settings: %v", err)
	}
	if _, err := db.db.Exec("DELETE FROM ClickLog"); err != nil {
		t.Fatalf("resetDB ClickLog: %v", err)
	}
}

func testLinx() []*Linx {
	return []*Linx{
		// Links
		{Type: LinxTypeLink, ShortName: "github", DestinationURL: "https://github.com", Description: "GitHub", Owner: "test@example.com"},
		{Type: LinxTypeLink, ShortName: "google", DestinationURL: "https://google.com", Description: "Google", Owner: "test@example.com"},
		{Type: LinxTypeLink, ShortName: "docs", DestinationURL: "https://docs.google.com/", Description: "Google Docs", Owner: "test@example.com"},
		// Chain: chain-a → chain-b → chain-c (external)
		{Type: LinxTypeLink, ShortName: "chain-a", DestinationURL: "/chain-b", Owner: "test@example.com"},
		{Type: LinxTypeLink, ShortName: "chain-b", DestinationURL: "/chain-c", Owner: "test@example.com"},
		{Type: LinxTypeLink, ShortName: "chain-c", DestinationURL: "https://example.com/final", Owner: "test@example.com"},
		// Template URLs
		{Type: LinxTypeLink, ShortName: "search", DestinationURL: "https://www.google.com/search?q={{.Path}}", Description: "Google search", Owner: "test@example.com"},
		{Type: LinxTypeLink, ShortName: "myprofile", DestinationURL: "https://corp.example.com/{{.User}}", Owner: "test@example.com"},
		// People
		{Type: LinxTypeEmployee, ShortName: "john", FirstName: "John", LastName: "Doe", Title: "Engineer", Email: "john@example.com", Phone: "555-1234", Owner: "test@example.com"},
		{Type: LinxTypeCustomer, ShortName: "acme", FirstName: "Acme", LastName: "Corp", Email: "contact@acme.com", Owner: "test@example.com"},
		{Type: LinxTypeVendor, ShortName: "vendor1", FirstName: "Vendor", LastName: "One", Email: "v1@vendor.com", Owner: "test@example.com"},
		// Documents
		{Type: LinxTypeDocument, ShortName: "readme", Description: "Project README", Owner: "test@example.com"},
	}
}

func seedTestData(t *testing.T) map[string]int64 {
	t.Helper()
	ids := make(map[string]int64)
	for _, c := range testLinx() {
		id, err := db.Save(c)
		if err != nil {
			t.Fatalf("seedTestData(%s): %v", c.ShortName, err)
		}
		ids[c.ShortName] = id
	}
	return ids
}

func doJSON(t *testing.T, mux http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf *bytes.Buffer
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		buf = bytes.NewBuffer(b)
	} else {
		buf = &bytes.Buffer{}
	}
	req := httptest.NewRequest(method, path, buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

func doRequest(t *testing.T, mux http.Handler, method, url string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, url, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// ---------------------------------------------------------------------------
// 1. Pure function tests
// ---------------------------------------------------------------------------

func TestExpandLink(t *testing.T) {
	tests := []struct {
		name    string
		long    string
		env     expandEnv
		want    string
		wantErr bool
	}{
		{
			name: "plain URL no path",
			long: "https://github.com",
			env:  expandEnv{Now: time.Now().UTC()},
			want: "https://github.com",
		},
		{
			name: "plain URL with path",
			long: "https://github.com",
			env:  expandEnv{Now: time.Now().UTC(), Path: "anthropics/claude"},
			want: "https://github.com/anthropics/claude",
		},
		{
			name: "trailing slash with path",
			long: "https://docs.google.com/",
			env:  expandEnv{Now: time.Now().UTC(), Path: "extra"},
			want: "https://docs.google.com/extra",
		},
		{
			name: "trailing slash no path",
			long: "https://docs.google.com/",
			env:  expandEnv{Now: time.Now().UTC()},
			want: "https://docs.google.com/",
		},
		{
			name: "template with .Path",
			long: "https://www.google.com/search?q={{.Path}}",
			env:  expandEnv{Now: time.Now().UTC(), Path: "test query"},
			want: "https://www.google.com/search?q=test query",
		},
		{
			name: "template with .User",
			long: "https://corp.example.com/{{.User}}",
			env:  expandEnv{Now: time.Now().UTC(), user: "alice"},
			want: "https://corp.example.com/alice",
		},
		{
			name:    "template with .User no user",
			long:    "https://corp.example.com/{{.User}}",
			env:     expandEnv{Now: time.Now().UTC(), user: ""},
			wantErr: true,
		},
		{
			name: "query merge",
			long: "https://site.com",
			env:  expandEnv{Now: time.Now().UTC(), query: url.Values{"tab": {"repos"}}},
			want: "https://site.com?tab=repos",
		},
		{
			name: "PathEscape func",
			long: "https://host.com/{{PathEscape .Path}}",
			env:  expandEnv{Now: time.Now().UTC(), Path: "a/b"},
			want: "https://host.com/a%2Fb",
		},
		{
			name: "QueryEscape func",
			long: "https://host.com/?q={{QueryEscape .Path}}",
			env:  expandEnv{Now: time.Now().UTC(), Path: "a b"},
			want: "https://host.com/?q=a+b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := expandLink(tt.long, tt.env)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.String() != tt.want {
				t.Errorf("got %q, want %q", got.String(), tt.want)
			}
		})
	}
}

func TestExtractLocalShortName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"/foo", "foo"},
		{"/foo/bar", ""},
		{"/", ""},
		{"", ""},
		{"https://external.com/single", "single"},
		{"https://external.com/a/b", ""},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			u, _ := url.Parse(tt.input)
			got := extractLocalShortName(u)
			if got != tt.want {
				t.Errorf("extractLocalShortName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestDetectLinkLoop(t *testing.T) {
	t.Run("external URL no loop", func(t *testing.T) {
		resetDB(t)
		seedTestData(t)
		msg := detectLinkLoop("github", "https://external.com")
		if msg != "" {
			t.Errorf("expected no loop, got %q", msg)
		}
	})

	t.Run("direct self-loop", func(t *testing.T) {
		resetDB(t)
		msg := detectLinkLoop("foo", "/foo")
		if msg == "" || !strings.Contains(msg, "loop") {
			t.Errorf("expected loop message, got %q", msg)
		}
	})

	t.Run("two-hop loop", func(t *testing.T) {
		resetDB(t)
		db.Save(&Linx{Type: LinxTypeLink, ShortName: "loopA", DestinationURL: "/loopB", Owner: "test@example.com"})
		msg := detectLinkLoop("loopB", "/loopA")
		if msg == "" || !strings.Contains(msg, "loop") {
			t.Errorf("expected loop message, got %q", msg)
		}
	})

	t.Run("chain ending external no loop", func(t *testing.T) {
		resetDB(t)
		db.Save(&Linx{Type: LinxTypeLink, ShortName: "extA", DestinationURL: "/extB", Owner: "test@example.com"})
		db.Save(&Linx{Type: LinxTypeLink, ShortName: "extB", DestinationURL: "https://example.com", Owner: "test@example.com"})
		msg := detectLinkLoop("newlink", "/extA")
		if msg != "" {
			t.Errorf("expected no loop, got %q", msg)
		}
	})

	t.Run("non-link linx breaks chain", func(t *testing.T) {
		resetDB(t)
		db.Save(&Linx{Type: LinxTypeEmployee, ShortName: "emp1", FirstName: "Test", Owner: "test@example.com"})
		msg := detectLinkLoop("newlink", "/emp1")
		if msg != "" {
			t.Errorf("expected no loop (person linx), got %q", msg)
		}
	})
}

// ---------------------------------------------------------------------------
// 2. Database layer tests
// ---------------------------------------------------------------------------

func TestDB_SaveAndLoad(t *testing.T) {
	resetDB(t)

	lnx := &Linx{Type: LinxTypeLink, ShortName: "dbtest1", DestinationURL: "https://example.com", Owner: "test@example.com"}
	id, err := db.Save(lnx)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive ID, got %d", id)
	}

	// LoadByID
	loaded, err := db.LoadByID(id)
	if err != nil {
		t.Fatalf("LoadByID: %v", err)
	}
	if loaded.ShortName != "dbtest1" {
		t.Errorf("ShortName = %q, want %q", loaded.ShortName, "dbtest1")
	}
	if loaded.DestinationURL != "https://example.com" {
		t.Errorf("DestinationURL = %q, want %q", loaded.DestinationURL, "https://example.com")
	}

	// LoadByShortName (case-insensitive)
	loaded2, err := db.LoadByShortName("DBTEST1")
	if err != nil {
		t.Fatalf("LoadByShortName: %v", err)
	}
	if loaded2.ID != id {
		t.Errorf("case-insensitive lookup returned different ID: %d vs %d", loaded2.ID, id)
	}

	// Not found
	_, err = db.LoadByID(999999)
	if err != fs.ErrNotExist {
		t.Errorf("LoadByID(999999) = %v, want fs.ErrNotExist", err)
	}
	_, err = db.LoadByShortName("nonexistent")
	if err != fs.ErrNotExist {
		t.Errorf("LoadByShortName(nonexistent) = %v, want fs.ErrNotExist", err)
	}
}

func TestDB_Update(t *testing.T) {
	resetDB(t)

	id, _ := db.Save(&Linx{Type: LinxTypeLink, ShortName: "upd1", DestinationURL: "https://old.com", Owner: "test@example.com"})
	err := db.Update(&Linx{ID: id, Type: LinxTypeLink, ShortName: "upd1", DestinationURL: "https://new.com", Owner: "test@example.com"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	loaded, _ := db.LoadByID(id)
	if loaded.DestinationURL != "https://new.com" {
		t.Errorf("DestinationURL after update = %q, want %q", loaded.DestinationURL, "https://new.com")
	}

	// Update non-existent
	err = db.Update(&Linx{ID: 999999, Type: LinxTypeLink, ShortName: "nope"})
	if err != fs.ErrNotExist {
		t.Errorf("Update(999999) = %v, want fs.ErrNotExist", err)
	}
}

func TestDB_Delete(t *testing.T) {
	resetDB(t)

	id, _ := db.Save(&Linx{Type: LinxTypeLink, ShortName: "del1", DestinationURL: "https://example.com", Owner: "test@example.com"})
	if err := db.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// LoadByID still finds it (soft-deleted).
	lnx, err := db.LoadByID(id)
	if err != nil {
		t.Fatalf("LoadByID after soft delete: %v", err)
	}
	if lnx.DeletedAt == 0 {
		t.Error("expected DeletedAt > 0 after soft delete")
	}
	// LoadByShortName should NOT find it.
	_, err = db.LoadByShortName("del1")
	if err != fs.ErrNotExist {
		t.Errorf("LoadByShortName after delete = %v, want fs.ErrNotExist", err)
	}

	// Double delete
	if err := db.Delete(id); err != fs.ErrNotExist {
		t.Errorf("double Delete = %v, want fs.ErrNotExist", err)
	}
}

func TestDB_Restore(t *testing.T) {
	resetDB(t)

	id, _ := db.Save(&Linx{Type: LinxTypeLink, ShortName: "rest1", DestinationURL: "https://example.com", Owner: "test@example.com"})
	db.Delete(id)

	if err := db.Restore(id); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	lnx, _ := db.LoadByID(id)
	if lnx.DeletedAt != 0 {
		t.Error("expected DeletedAt = 0 after restore")
	}
	if _, err := db.LoadByShortName("rest1"); err != nil {
		t.Errorf("LoadByShortName after restore: %v", err)
	}
	// Restore non-deleted should fail.
	if err := db.Restore(id); err != fs.ErrNotExist {
		t.Errorf("Restore active item = %v, want fs.ErrNotExist", err)
	}
}

func TestDB_LoadDeleted(t *testing.T) {
	resetDB(t)
	seedTestData(t)

	deleted, _ := db.LoadDeleted()
	if len(deleted) != 0 {
		t.Errorf("LoadDeleted before any delete = %d, want 0", len(deleted))
	}

	lnx, _ := db.LoadByShortName("github")
	db.Delete(lnx.ID)
	deleted, _ = db.LoadDeleted()
	if len(deleted) != 1 || deleted[0].ShortName != "github" {
		t.Errorf("LoadDeleted = %d, want 1 (github)", len(deleted))
	}
}

func TestDB_PurgeDeleted(t *testing.T) {
	resetDB(t)

	id, _ := db.Save(&Linx{Type: LinxTypeLink, ShortName: "purge1", DestinationURL: "https://example.com", Owner: "test@example.com"})
	db.Delete(id)

	n, err := db.PurgeDeleted(time.Now().Add(time.Hour).Unix())
	if err != nil {
		t.Fatalf("PurgeDeleted: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d, want 1", n)
	}
	_, err = db.LoadByID(id)
	if err != fs.ErrNotExist {
		t.Errorf("LoadByID after purge = %v, want fs.ErrNotExist", err)
	}
}

func TestDB_SaveReusesDeletedShortName(t *testing.T) {
	resetDB(t)

	id1, _ := db.Save(&Linx{Type: LinxTypeLink, ShortName: "reuse", DestinationURL: "https://old.com", Owner: "test@example.com"})
	db.Delete(id1)

	id2, err := db.Save(&Linx{Type: LinxTypeLink, ShortName: "reuse", DestinationURL: "https://new.com", Owner: "test@example.com"})
	if err != nil {
		t.Fatalf("Save reuse: %v", err)
	}
	if id2 == id1 {
		t.Error("expected new ID, got same as deleted item")
	}
	lnx, _ := db.LoadByShortName("reuse")
	if lnx.DestinationURL != "https://new.com" {
		t.Errorf("destination = %s, want https://new.com", lnx.DestinationURL)
	}
}

func TestDB_LoadAll(t *testing.T) {
	resetDB(t)
	seedTestData(t)

	// All linx
	all, err := db.LoadAll("")
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(all) != len(testLinx()) {
		t.Errorf("LoadAll() returned %d linx, want %d", len(all), len(testLinx()))
	}

	// Filter by type
	links, err := db.LoadAll(LinxTypeLink)
	if err != nil {
		t.Fatalf("LoadAll(link): %v", err)
	}
	for _, c := range links {
		if c.Type != LinxTypeLink {
			t.Errorf("filter returned non-link linx: %s (type %s)", c.ShortName, c.Type)
		}
	}
	if len(links) != 8 { // github, google, docs, chain-a/b/c, search, myprofile
		t.Errorf("LoadAll(link) = %d linx, want 8", len(links))
	}

	employees, err := db.LoadAll(LinxTypeEmployee)
	if err != nil {
		t.Fatalf("LoadAll(employee): %v", err)
	}
	if len(employees) != 1 {
		t.Errorf("LoadAll(employee) = %d linx, want 1", len(employees))
	}
}

func TestDB_IncrementClick(t *testing.T) {
	resetDB(t)
	db.Save(&Linx{Type: LinxTypeLink, ShortName: "clicks1", DestinationURL: "https://example.com", Owner: "test@example.com"})

	if err := db.IncrementClick("clicks1"); err != nil {
		t.Fatalf("IncrementClick: %v", err)
	}
	lnx, _ := db.LoadByShortName("clicks1")
	if lnx.ClickCount != 1 {
		t.Errorf("ClickCount = %d, want 1", lnx.ClickCount)
	}
	if lnx.LastClicked == 0 {
		t.Error("LastClicked should be non-zero after click")
	}

	// Second click
	db.IncrementClick("clicks1")
	lnx, _ = db.LoadByShortName("clicks1")
	if lnx.ClickCount != 2 {
		t.Errorf("ClickCount = %d, want 2", lnx.ClickCount)
	}
}

func TestDB_LinxCount(t *testing.T) {
	resetDB(t)

	count, err := db.LinxCount("")
	if err != nil {
		t.Fatalf("CardCount: %v", err)
	}
	if count != 0 {
		t.Errorf("empty DB count = %d, want 0", count)
	}

	seedTestData(t)
	count, _ = db.LinxCount("")
	if count != len(testLinx()) {
		t.Errorf("total count = %d, want %d", count, len(testLinx()))
	}

	linkCount, _ := db.LinxCount(LinxTypeLink)
	if linkCount != 8 {
		t.Errorf("link count = %d, want 8", linkCount)
	}
}

// ---------------------------------------------------------------------------
// 3. API handler tests
// ---------------------------------------------------------------------------

func TestAPI_LinxList(t *testing.T) {
	mux := serveHandler()

	t.Run("empty database", func(t *testing.T) {
		resetDB(t)
		w := doRequest(t, mux, "GET", "/api/linx")
		if w.Code != 200 {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		var items []Linx
		json.Unmarshal(w.Body.Bytes(), &items)
		if len(items) != 0 {
			t.Errorf("expected empty array, got %d linx", len(items))
		}
	})

	t.Run("all linx", func(t *testing.T) {
		resetDB(t)
		seedTestData(t)
		w := doRequest(t, mux, "GET", "/api/linx")
		if w.Code != 200 {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		var items []Linx
		json.Unmarshal(w.Body.Bytes(), &items)
		if len(items) != len(testLinx()) {
			t.Errorf("got %d linx, want %d", len(items), len(testLinx()))
		}
	})

	t.Run("filter by type=link", func(t *testing.T) {
		resetDB(t)
		seedTestData(t)
		w := doRequest(t, mux, "GET", "/api/linx?type=link")
		var items []Linx
		json.Unmarshal(w.Body.Bytes(), &items)
		for _, c := range items {
			if c.Type != LinxTypeLink {
				t.Errorf("got non-link linx %s (type %s)", c.ShortName, c.Type)
			}
		}
	})

	t.Run("filter by type=employee", func(t *testing.T) {
		resetDB(t)
		seedTestData(t)
		w := doRequest(t, mux, "GET", "/api/linx?type=employee")
		var items []Linx
		json.Unmarshal(w.Body.Bytes(), &items)
		if len(items) != 1 {
			t.Errorf("got %d employees, want 1", len(items))
		}
	})

	t.Run("invalid type", func(t *testing.T) {
		resetDB(t)
		w := doRequest(t, mux, "GET", "/api/linx?type=bogus")
		if w.Code != 400 {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})
}

func TestAPI_LinxCreate(t *testing.T) {
	mux := serveHandler()

	t.Run("valid link", func(t *testing.T) {
		resetDB(t)
		w := doJSON(t, mux, "POST", "/api/linx", map[string]string{
			"type": "link", "shortName": "newlink", "destinationURL": "https://example.com",
		})
		if w.Code != 201 {
			t.Fatalf("status = %d, want 201; body: %s", w.Code, w.Body.String())
		}
		var lnx Linx
		json.Unmarshal(w.Body.Bytes(), &lnx)
		if lnx.ShortName != "newlink" {
			t.Errorf("shortName = %q, want %q", lnx.ShortName, "newlink")
		}
		if lnx.Owner != "test@example.com" {
			t.Errorf("owner = %q, want auto-set to test@example.com", lnx.Owner)
		}
	})

	t.Run("valid employee", func(t *testing.T) {
		resetDB(t)
		w := doJSON(t, mux, "POST", "/api/linx", map[string]string{
			"type": "employee", "shortName": "newemp", "firstName": "Test",
		})
		if w.Code != 201 {
			t.Fatalf("status = %d, want 201; body: %s", w.Code, w.Body.String())
		}
	})

	t.Run("type defaults to link", func(t *testing.T) {
		resetDB(t)
		w := doJSON(t, mux, "POST", "/api/linx", map[string]string{
			"shortName": "deftype", "destinationURL": "https://example.com",
		})
		if w.Code != 201 {
			t.Fatalf("status = %d, want 201", w.Code)
		}
		var lnx Linx
		json.Unmarshal(w.Body.Bytes(), &lnx)
		if lnx.Type != LinxTypeLink {
			t.Errorf("type = %q, want %q", lnx.Type, LinxTypeLink)
		}
	})

	t.Run("missing shortName", func(t *testing.T) {
		resetDB(t)
		w := doJSON(t, mux, "POST", "/api/linx", map[string]string{
			"type": "link", "destinationURL": "https://example.com",
		})
		if w.Code != 400 {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})

	t.Run("invalid shortName", func(t *testing.T) {
		resetDB(t)
		w := doJSON(t, mux, "POST", "/api/linx", map[string]string{
			"type": "link", "shortName": "has spaces", "destinationURL": "https://example.com",
		})
		if w.Code != 400 {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})

	t.Run("missing destinationURL for link", func(t *testing.T) {
		resetDB(t)
		w := doJSON(t, mux, "POST", "/api/linx", map[string]string{
			"type": "link", "shortName": "nourl",
		})
		if w.Code != 400 {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})

	t.Run("invalid URL scheme", func(t *testing.T) {
		resetDB(t)
		w := doJSON(t, mux, "POST", "/api/linx", map[string]string{
			"type": "link", "shortName": "badurl", "destinationURL": "ftp://bad.com",
		})
		if w.Code != 400 {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})

	t.Run("loop detection", func(t *testing.T) {
		resetDB(t)
		// Create loopX first, then try to create loopY pointing to itself.
		// Create a link, then try to create one pointing to itself.
		doJSON(t, mux, "POST", "/api/linx", map[string]string{
			"type": "link", "shortName": "loopX", "destinationURL": "https://example.com",
		})
		w := doJSON(t, mux, "POST", "/api/linx", map[string]string{
			"type": "link", "shortName": "loopY", "destinationURL": "http://localhost/loopY",
		})
		if w.Code != 400 {
			t.Errorf("status = %d, want 400 for self-loop", w.Code)
		}
		if !strings.Contains(w.Body.String(), "loop") {
			t.Errorf("body should mention loop: %s", w.Body.String())
		}
	})

	t.Run("missing firstName for person", func(t *testing.T) {
		resetDB(t)
		w := doJSON(t, mux, "POST", "/api/linx", map[string]string{
			"type": "employee", "shortName": "nofirst",
		})
		if w.Code != 400 {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})

	t.Run("duplicate shortName", func(t *testing.T) {
		resetDB(t)
		doJSON(t, mux, "POST", "/api/linx", map[string]string{
			"type": "link", "shortName": "dup", "destinationURL": "https://example.com",
		})
		w := doJSON(t, mux, "POST", "/api/linx", map[string]string{
			"type": "link", "shortName": "dup", "destinationURL": "https://other.com",
		})
		if w.Code != 409 {
			t.Errorf("status = %d, want 409", w.Code)
		}
	})
}

func TestAPI_LinxColor(t *testing.T) {
	mux := serveHandler()

	t.Run("color round-trip", func(t *testing.T) {
		resetDB(t)
		w := doJSON(t, mux, "POST", "/api/linx", map[string]string{
			"type": "link", "shortName": "colored", "destinationURL": "https://example.com",
			"color": "#ef4444",
		})
		if w.Code != 201 {
			t.Fatalf("create status = %d, want 201; body: %s", w.Code, w.Body.String())
		}
		var lnx Linx
		json.Unmarshal(w.Body.Bytes(), &lnx)
		if lnx.Color != "#ef4444" {
			t.Errorf("color = %q, want %q", lnx.Color, "#ef4444")
		}

		// Update color
		w = doJSON(t, mux, "PUT", fmt.Sprintf("/api/linx/%d", lnx.ID), map[string]string{
			"type": "link", "shortName": "colored", "destinationURL": "https://example.com",
			"color": "#3b82f6",
		})
		if w.Code != 200 {
			t.Fatalf("update status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		json.Unmarshal(w.Body.Bytes(), &lnx)
		if lnx.Color != "#3b82f6" {
			t.Errorf("color = %q, want %q", lnx.Color, "#3b82f6")
		}

		// Clear color
		w = doJSON(t, mux, "PUT", fmt.Sprintf("/api/linx/%d", lnx.ID), map[string]string{
			"type": "link", "shortName": "colored", "destinationURL": "https://example.com",
			"color": "",
		})
		if w.Code != 200 {
			t.Fatalf("clear status = %d, want 200", w.Code)
		}
		json.Unmarshal(w.Body.Bytes(), &lnx)
		if lnx.Color != "" {
			t.Errorf("color = %q, want empty", lnx.Color)
		}
	})
}

func TestAPI_LinxUpdate(t *testing.T) {
	mux := serveHandler()

	t.Run("valid update", func(t *testing.T) {
		resetDB(t)
		ids := seedTestData(t)
		id := ids["github"]
		w := doJSON(t, mux, "PUT", fmt.Sprintf("/api/linx/%d", id), map[string]string{
			"shortName": "github", "destinationURL": "https://github.com/new", "type": "link",
		})
		if w.Code != 200 {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		var lnx Linx
		json.Unmarshal(w.Body.Bytes(), &lnx)
		if lnx.DestinationURL != "https://github.com/new" {
			t.Errorf("destinationURL = %q, want updated value", lnx.DestinationURL)
		}
	})

	t.Run("non-existent ID", func(t *testing.T) {
		resetDB(t)
		w := doJSON(t, mux, "PUT", "/api/linx/999999", map[string]string{
			"shortName": "x", "destinationURL": "https://x.com", "type": "link",
		})
		if w.Code != 404 {
			t.Errorf("status = %d, want 404", w.Code)
		}
	})

	t.Run("invalid ID", func(t *testing.T) {
		resetDB(t)
		w := doJSON(t, mux, "PUT", "/api/linx/abc", map[string]string{
			"shortName": "x", "type": "link",
		})
		if w.Code != 400 {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})

	t.Run("loop detection on update", func(t *testing.T) {
		resetDB(t)
		ids := seedTestData(t)
		id := ids["github"]
		w := doJSON(t, mux, "PUT", fmt.Sprintf("/api/linx/%d", id), map[string]string{
			"shortName": "github", "destinationURL": "/github", "type": "link",
		})
		if w.Code != 400 {
			t.Errorf("status = %d, want 400 for self-loop", w.Code)
		}
	})
}

func TestAPI_LinxDelete(t *testing.T) {
	mux := serveHandler()

	t.Run("valid delete", func(t *testing.T) {
		resetDB(t)
		ids := seedTestData(t)
		id := ids["google"]
		w := doRequest(t, mux, "DELETE", fmt.Sprintf("/api/linx/%d", id))
		if w.Code != 200 {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		var resp map[string]bool
		json.Unmarshal(w.Body.Bytes(), &resp)
		if !resp["ok"] {
			t.Error("expected ok:true")
		}
	})

	t.Run("not found", func(t *testing.T) {
		resetDB(t)
		w := doRequest(t, mux, "DELETE", "/api/linx/999999")
		if w.Code != 404 {
			t.Errorf("status = %d, want 404", w.Code)
		}
	})

	t.Run("invalid ID", func(t *testing.T) {
		resetDB(t)
		w := doRequest(t, mux, "DELETE", "/api/linx/abc")
		if w.Code != 400 {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})
}

func TestAPI_DBExportImport(t *testing.T) {
	mux := serveHandler()

	t.Run("GET empty", func(t *testing.T) {
		resetDB(t)
		w := doRequest(t, mux, "GET", "/api/db")
		if w.Code != 200 {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		var items []Linx
		json.Unmarshal(w.Body.Bytes(), &items)
		if len(items) != 0 {
			t.Errorf("expected empty, got %d", len(items))
		}
	})

	t.Run("GET with data", func(t *testing.T) {
		resetDB(t)
		seedTestData(t)
		w := doRequest(t, mux, "GET", "/api/db")
		var items []Linx
		json.Unmarshal(w.Body.Bytes(), &items)
		if len(items) != len(testLinx()) {
			t.Errorf("got %d, want %d", len(items), len(testLinx()))
		}
	})

	t.Run("PUT import new", func(t *testing.T) {
		resetDB(t)
		imports := []map[string]string{
			{"type": "link", "shortName": "imp1", "destinationURL": "https://imp1.com"},
			{"type": "link", "shortName": "imp2", "destinationURL": "https://imp2.com"},
		}
		w := doJSON(t, mux, "PUT", "/api/db", imports)
		if w.Code != 200 {
			t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
		}
		var resp map[string]int
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["added"] != 2 {
			t.Errorf("added = %d, want 2", resp["added"])
		}
		if resp["skipped"] != 0 {
			t.Errorf("skipped = %d, want 0", resp["skipped"])
		}
	})

	t.Run("PUT import skips existing", func(t *testing.T) {
		resetDB(t)
		db.Save(&Linx{Type: LinxTypeLink, ShortName: "existing", DestinationURL: "https://existing.com", Owner: "test@example.com"})
		imports := []map[string]string{
			{"type": "link", "shortName": "existing", "destinationURL": "https://new.com"},
			{"type": "link", "shortName": "brand-new", "destinationURL": "https://brandnew.com"},
		}
		w := doJSON(t, mux, "PUT", "/api/db", imports)
		var resp map[string]int
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["added"] != 1 || resp["skipped"] != 1 {
			t.Errorf("got added=%d skipped=%d, want added=1 skipped=1", resp["added"], resp["skipped"])
		}
	})

	t.Run("PUT invalid JSON", func(t *testing.T) {
		resetDB(t)
		req := httptest.NewRequest("PUT", "/api/db", strings.NewReader("not json"))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 400 {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})
}

func TestAPI_WhoAmI(t *testing.T) {
	mux := serveHandler()
	w := doRequest(t, mux, "GET", "/api/whoami")
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["login"] != "test@example.com" {
		t.Errorf("login = %q, want %q", resp["login"], "test@example.com")
	}
}

func TestIsLocalhost(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		want       bool
	}{
		{"IPv4 loopback", "127.0.0.1:54321", true},
		{"IPv6 loopback", "[::1]:54321", true},
		{"LAN address", "192.168.1.100:54321", false},
		{"Tailscale address", "100.64.0.1:54321", false},
		{"httptest default", "192.0.2.1:1234", false},
		{"bare IPv4 loopback", "127.0.0.1", true},
		{"bare IPv6 loopback", "::1", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &http.Request{RemoteAddr: tt.remoteAddr}
			if got := isLocalhost(r); got != tt.want {
				t.Errorf("isLocalhost(%q) = %v, want %v", tt.remoteAddr, got, tt.want)
			}
		})
	}
}

func TestLocalhostAutoAdmin(t *testing.T) {
	resetDB(t)
	db.Save(&Linx{Type: LinxTypeLink, ShortName: "priv", DestinationURL: "https://example.com", Owner: "other@example.com"})

	origCurrentUser := currentUser
	defer func() { currentUser = origCurrentUser }()
	currentUser = func(r *http.Request) (string, bool, error) {
		return "local@testhost", false, nil
	}

	t.Run("localhost can edit others linx", func(t *testing.T) {
		r := &http.Request{RemoteAddr: "127.0.0.1:54321"}
		if !canEdit(r, "other@example.com") {
			t.Error("localhost should have auto-admin access")
		}
	})
	t.Run("non-localhost cannot edit others linx", func(t *testing.T) {
		r := &http.Request{RemoteAddr: "192.168.1.100:54321"}
		if canEdit(r, "other@example.com") {
			t.Error("non-localhost should not have auto-admin access")
		}
	})
	t.Run("non-localhost can edit own linx", func(t *testing.T) {
		r := &http.Request{RemoteAddr: "192.168.1.100:54321"}
		currentUser = func(r *http.Request) (string, bool, error) {
			return "other@example.com", false, nil
		}
		if !canEdit(r, "other@example.com") {
			t.Error("owner should always be able to edit own linx")
		}
	})
}

func TestGrantsAdmin_CanEdit(t *testing.T) {
	resetDB(t)
	db.Save(&Linx{Type: LinxTypeLink, ShortName: "priv", DestinationURL: "https://example.com", Owner: "other@example.com"})

	origCurrentUser := currentUser
	defer func() { currentUser = origCurrentUser }()

	t.Run("grant admin with adminMode can edit others linx", func(t *testing.T) {
		currentUser = func(r *http.Request) (string, bool, error) {
			return "admin@example.com", true, nil
		}
		// Admin mode must be toggled on to bypass ownership.
		db.PutSetting("admin@example.com", "adminMode", "true")
		r := &http.Request{RemoteAddr: "100.64.0.1:54321"}
		if !canEdit(r, "other@example.com") {
			t.Error("grant admin with adminMode should be able to edit others linx")
		}
	})
	t.Run("grant admin without adminMode cannot edit others linx", func(t *testing.T) {
		currentUser = func(r *http.Request) (string, bool, error) {
			return "admin@example.com", true, nil
		}
		db.PutSetting("admin@example.com", "adminMode", "false")
		r := &http.Request{RemoteAddr: "100.64.0.1:54321"}
		if canEdit(r, "other@example.com") {
			t.Error("grant admin without adminMode should not bypass ownership")
		}
	})
	t.Run("non-admin cannot edit others linx", func(t *testing.T) {
		currentUser = func(r *http.Request) (string, bool, error) {
			return "regular@example.com", false, nil
		}
		r := &http.Request{RemoteAddr: "100.64.0.1:54321"}
		if canEdit(r, "other@example.com") {
			t.Error("non-admin should not be able to edit others linx")
		}
	})
}

func TestAPI_WhoAmI_LocalhostAdmin(t *testing.T) {
	origCurrentUser := currentUser
	defer func() { currentUser = origCurrentUser }()
	currentUser = func(r *http.Request) (string, bool, error) {
		return "local@testhost", false, nil
	}
	mux := serveHandler()

	t.Run("localhost gets localhostAdmin true", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/whoami", nil)
		req.RemoteAddr = "127.0.0.1:54321"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		var resp map[string]any
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["localhostAdmin"] != true {
			t.Errorf("localhostAdmin = %v, want true", resp["localhostAdmin"])
		}
		if resp["isAdmin"] != true {
			t.Errorf("isAdmin = %v, want true", resp["isAdmin"])
		}
	})
	t.Run("non-localhost gets localhostAdmin false", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/whoami", nil)
		req.RemoteAddr = "192.168.1.100:54321"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		var resp map[string]any
		json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["localhostAdmin"] == true {
			t.Errorf("localhostAdmin = %v, want false", resp["localhostAdmin"])
		}
	})
}

func TestHasUserPerm(t *testing.T) {
	orig := userPerms
	defer func() { userPerms = orig }()

	t.Run("wildcard grants all", func(t *testing.T) {
		userPerms = []string{"*"}
		if !hasUserPerm("add") || !hasUserPerm("update") || !hasUserPerm("delete") {
			t.Error("wildcard should grant all permissions")
		}
	})
	t.Run("specific perms", func(t *testing.T) {
		userPerms = []string{"add"}
		if !hasUserPerm("add") {
			t.Error("should have add perm")
		}
		if hasUserPerm("update") || hasUserPerm("delete") {
			t.Error("should not have update or delete perm")
		}
	})
	t.Run("empty denies all", func(t *testing.T) {
		userPerms = []string{}
		if hasUserPerm("add") || hasUserPerm("update") || hasUserPerm("delete") {
			t.Error("empty perms should deny all")
		}
	})
	t.Run("case insensitive", func(t *testing.T) {
		userPerms = []string{"Add", "DELETE"}
		if !hasUserPerm("add") || !hasUserPerm("delete") {
			t.Error("perms should be case insensitive")
		}
	})
}

func TestUserPerms_Create(t *testing.T) {
	resetDB(t)
	origPerms := userPerms
	origUser := currentUser
	defer func() { userPerms = origPerms; currentUser = origUser }()

	currentUser = func(r *http.Request) (string, bool, error) {
		return "local@testhost", false, nil
	}
	mux := serveHandler()

	t.Run("denied without add perm", func(t *testing.T) {
		userPerms = []string{}
		body := `{"type":"link","shortName":"blocked","destinationURL":"https://example.com"}`
		req := httptest.NewRequest("POST", "/api/linx", strings.NewReader(body))
		req.RemoteAddr = "192.168.1.100:54321"
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Errorf("status = %d, want 403", w.Code)
		}
	})
	t.Run("allowed with add perm", func(t *testing.T) {
		userPerms = []string{"add"}
		body := `{"type":"link","shortName":"allowed","destinationURL":"https://example.com"}`
		req := httptest.NewRequest("POST", "/api/linx", strings.NewReader(body))
		req.RemoteAddr = "192.168.1.100:54321"
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 201 {
			t.Errorf("status = %d, want 201", w.Code)
		}
	})
	t.Run("localhost bypasses perms", func(t *testing.T) {
		userPerms = []string{}
		body := `{"type":"link","shortName":"fromlocal","destinationURL":"https://example.com"}`
		req := httptest.NewRequest("POST", "/api/linx", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:54321"
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 201 {
			t.Errorf("status = %d, want 201 (localhost should bypass perms)", w.Code)
		}
	})
}

func TestUserPerms_Update(t *testing.T) {
	resetDB(t)
	origPerms := userPerms
	origUser := currentUser
	defer func() { userPerms = origPerms; currentUser = origUser }()

	currentUser = func(r *http.Request) (string, bool, error) {
		return "local@testhost", false, nil
	}
	lnx := &Linx{Type: LinxTypeLink, ShortName: "uptest", DestinationURL: "https://example.com", Owner: "local@testhost"}
	id, _ := db.Save(lnx)
	mux := serveHandler()

	t.Run("denied without update perm", func(t *testing.T) {
		userPerms = []string{"add"}
		body := `{"type":"link","shortName":"uptest","destinationURL":"https://changed.com"}`
		req := httptest.NewRequest("PUT", fmt.Sprintf("/api/linx/%d", id), strings.NewReader(body))
		req.RemoteAddr = "192.168.1.100:54321"
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Errorf("status = %d, want 403", w.Code)
		}
	})
	t.Run("allowed with update perm", func(t *testing.T) {
		userPerms = []string{"update"}
		body := `{"type":"link","shortName":"uptest","destinationURL":"https://changed.com"}`
		req := httptest.NewRequest("PUT", fmt.Sprintf("/api/linx/%d", id), strings.NewReader(body))
		req.RemoteAddr = "192.168.1.100:54321"
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Errorf("status = %d, want 200", w.Code)
		}
	})
}

func TestUserPerms_Delete(t *testing.T) {
	resetDB(t)
	origPerms := userPerms
	origUser := currentUser
	defer func() { userPerms = origPerms; currentUser = origUser }()

	currentUser = func(r *http.Request) (string, bool, error) {
		return "local@testhost", false, nil
	}
	lnx := &Linx{Type: LinxTypeLink, ShortName: "deltest", DestinationURL: "https://example.com", Owner: "local@testhost"}
	id, _ := db.Save(lnx)
	mux := serveHandler()

	t.Run("denied without delete perm", func(t *testing.T) {
		userPerms = []string{"add", "update"}
		req := httptest.NewRequest("DELETE", fmt.Sprintf("/api/linx/%d", id), nil)
		req.RemoteAddr = "192.168.1.100:54321"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Errorf("status = %d, want 403", w.Code)
		}
	})
	t.Run("allowed with delete perm", func(t *testing.T) {
		userPerms = []string{"delete"}
		req := httptest.NewRequest("DELETE", fmt.Sprintf("/api/linx/%d", id), nil)
		req.RemoteAddr = "192.168.1.100:54321"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Errorf("status = %d, want 200", w.Code)
		}
	})
}

// ---------------------------------------------------------------------------
// 4. Link resolution tests
// ---------------------------------------------------------------------------

func TestResolve_BasicRedirect(t *testing.T) {
	resetDB(t)
	seedTestData(t)
	mux := serveHandler()

	w := doRequest(t, mux, "GET", "/github")
	if w.Code != 302 {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	if loc != "https://github.com" {
		t.Errorf("Location = %q, want %q", loc, "https://github.com")
	}
}

func TestResolve_PathPassthrough(t *testing.T) {
	resetDB(t)
	seedTestData(t)
	mux := serveHandler()

	w := doRequest(t, mux, "GET", "/github/anthropics/claude")
	if w.Code != 302 {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	if loc != "https://github.com/anthropics/claude" {
		t.Errorf("Location = %q, want path passthrough", loc)
	}
}

func TestResolve_QueryPassthrough(t *testing.T) {
	resetDB(t)
	seedTestData(t)
	mux := serveHandler()

	w := doRequest(t, mux, "GET", "/github?tab=repos")
	if w.Code != 302 {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	u, _ := url.Parse(loc)
	if u.Query().Get("tab") != "repos" {
		t.Errorf("query not passed through: %q", loc)
	}
}

func TestResolve_TrailingSlashURL(t *testing.T) {
	resetDB(t)
	seedTestData(t)
	mux := serveHandler()

	w := doRequest(t, mux, "GET", "/docs/extra")
	if w.Code != 302 {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	if loc != "https://docs.google.com/extra" {
		t.Errorf("Location = %q, want trailing-slash passthrough", loc)
	}
}

func TestResolve_PunctuationTrim(t *testing.T) {
	resetDB(t)
	seedTestData(t)
	mux := serveHandler()

	suffixes := []string{".", ",", ")", "]", "}"}
	for _, s := range suffixes {
		t.Run("trailing_"+s, func(t *testing.T) {
			w := doRequest(t, mux, "GET", "/github"+s)
			if w.Code != 302 {
				t.Errorf("status = %d, want 302 for /github%s", w.Code, s)
			}
			loc := w.Header().Get("Location")
			if !strings.HasPrefix(loc, "https://github.com") {
				t.Errorf("Location = %q, want github redirect", loc)
			}
		})
	}
}

func TestResolve_DetailPage(t *testing.T) {
	resetDB(t)
	seedTestData(t)
	mux := serveHandler()

	t.Run("link detail", func(t *testing.T) {
		w := doRequest(t, mux, "GET", "/github+")
		if w.Code != 200 {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		ct := w.Header().Get("Content-Type")
		if !strings.Contains(ct, "text/html") {
			t.Errorf("Content-Type = %q, want text/html", ct)
		}
		body := w.Body.String()
		if !strings.Contains(body, "github") {
			t.Error("detail page should contain short name")
		}
		if !strings.Contains(body, "https://github.com") {
			t.Error("detail page should contain destination URL")
		}
	})

	t.Run("person detail", func(t *testing.T) {
		w := doRequest(t, mux, "GET", "/john+")
		if w.Code != 200 {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, "John") {
			t.Error("person detail should contain first name")
		}
	})

	t.Run("not found +", func(t *testing.T) {
		w := doRequest(t, mux, "GET", "/nonexistent+")
		if w.Code != 404 {
			t.Errorf("status = %d, want 404", w.Code)
		}
	})
}

func TestResolve_PersonLinx(t *testing.T) {
	resetDB(t)
	seedTestData(t)
	mux := serveHandler()

	tests := []struct {
		path     string
		contains string
	}{
		{"/john", "John"},
		{"/acme", "Acme"},
		{"/vendor1", "Vendor"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			w := doRequest(t, mux, "GET", tt.path)
			if w.Code != 200 {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			ct := w.Header().Get("Content-Type")
			if !strings.Contains(ct, "text/html") {
				t.Errorf("Content-Type = %q, want text/html", ct)
			}
			if !strings.Contains(w.Body.String(), tt.contains) {
				t.Errorf("body should contain %q", tt.contains)
			}
		})
	}
}

func TestResolve_Chain(t *testing.T) {
	resetDB(t)
	seedTestData(t)
	mux := serveHandler()

	// chain-a → chain-b → chain-c → https://example.com/final
	w := doRequest(t, mux, "GET", "/chain-a")
	if w.Code != 302 {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	if loc != "https://example.com/final" {
		t.Errorf("Location = %q, want https://example.com/final (chain should resolve)", loc)
	}
}

func TestResolve_TemplateURL(t *testing.T) {
	resetDB(t)
	seedTestData(t)
	mux := serveHandler()

	t.Run("template with path", func(t *testing.T) {
		w := doRequest(t, mux, "GET", "/search/hello")
		if w.Code != 302 {
			t.Fatalf("status = %d, want 302", w.Code)
		}
		loc := w.Header().Get("Location")
		if !strings.Contains(loc, "q=hello") {
			t.Errorf("Location = %q, want path in query param", loc)
		}
	})

	t.Run("template with .User", func(t *testing.T) {
		w := doRequest(t, mux, "GET", "/myprofile")
		if w.Code != 302 {
			t.Fatalf("status = %d, want 302", w.Code)
		}
		loc := w.Header().Get("Location")
		if loc != "https://corp.example.com/test@example.com" {
			t.Errorf("Location = %q, want user in URL", loc)
		}
	})
}

func TestResolve_MaxDepth(t *testing.T) {
	resetDB(t)
	// Create a chain: d1 → /d2, d2 → /d3, d3 → /d4, d4 → https://end.com
	db.Save(&Linx{Type: LinxTypeLink, ShortName: "d1", DestinationURL: "/d2", Owner: "test@example.com"})
	db.Save(&Linx{Type: LinxTypeLink, ShortName: "d2", DestinationURL: "/d3", Owner: "test@example.com"})
	db.Save(&Linx{Type: LinxTypeLink, ShortName: "d3", DestinationURL: "/d4", Owner: "test@example.com"})
	db.Save(&Linx{Type: LinxTypeLink, ShortName: "d4", DestinationURL: "https://end.com", Owner: "test@example.com"})

	old := *maxResolveDepth
	*maxResolveDepth = 1
	t.Cleanup(func() { *maxResolveDepth = old })

	mux := serveHandler()
	w := doRequest(t, mux, "GET", "/d1")
	if w.Code != 302 {
		t.Fatalf("status = %d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	// With depth=1, d1 expands to /d2, follows 1 hop to d2→/d3, then stops.
	// The final Location should be /d3 (a local path), NOT https://end.com.
	if loc == "https://end.com" {
		t.Error("maxResolveDepth=1 should prevent reaching end of 3-hop chain")
	}
}

func TestResolve_NotFound(t *testing.T) {
	resetDB(t)
	mux := serveHandler()

	w := doRequest(t, mux, "GET", "/nonexistent")
	if w.Code != 404 {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// ---------------------------------------------------------------------------
// 5. Static / misc handler tests
// ---------------------------------------------------------------------------

func TestServe_Index(t *testing.T) {
	mux := serveHandler()
	w := doRequest(t, mux, "GET", "/")
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
}

func TestServe_Favicon(t *testing.T) {
	mux := serveHandler()
	w := doRequest(t, mux, "GET", "/favicon.svg")
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "image/svg+xml") {
		t.Errorf("Content-Type = %q, want image/svg+xml", ct)
	}
}

func TestServe_Help(t *testing.T) {
	mux := serveHandler()
	w := doRequest(t, mux, "GET", "/.help")
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(w.Body.String(), "GoLinx") {
		t.Error("help page should contain GoLinx")
	}
}

func TestServe_Export(t *testing.T) {
	resetDB(t)
	seedTestData(t)
	mux := serveHandler()

	w := doRequest(t, mux, "GET", "/.export")
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	cd := w.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
	var items []Linx
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(items) != len(testLinx()) {
		t.Errorf("exported %d linx, want %d", len(items), len(testLinx()))
	}
}

// ── Listener Parsing ─────────────────────────────────────────────

func TestParseListener(t *testing.T) {
	t.Run("valid URIs", func(t *testing.T) {
		tests := []struct {
			name string
			raw  string
		}{
			{"http empty host", "http://:8080"},
			{"http ipv4 any", "http://0.0.0.0:8080"},
			{"http ipv4 loopback", "http://127.0.0.1:8080"},
			{"http ipv6 loopback", "http://[::1]:8080"},
			{"https with certs", "https://0.0.0.0:443;cert=c.pem;key=k.pem"},
			{"ts+https", "ts+https://:443"},
			{"ts+http", "ts+http://:8080"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := parseListener(tc.raw); err != nil {
					t.Errorf("parseListener(%q) failed: %v", tc.raw, err)
				}
			})
		}
	})

	t.Run("invalid URIs", func(t *testing.T) {
		tests := []struct {
			name string
			raw  string
		}{
			{"reject hostname http", "http://localhost:8080"},
			{"reject hostname ts+https", "ts+https://go:443"},
			{"reject hostname https", "https://myhost:443;cert=c.pem;key=k.pem"},
			{"missing scheme", ":8080"},
			{"unknown scheme", "ftp://:21"},
			{"old tailscale scheme", "tailscale://:443"},
			{"missing port", "http://"},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := parseListener(tc.raw); err == nil {
					t.Errorf("parseListener(%q) succeeded, want error", tc.raw)
				}
			})
		}
	})

	t.Run("scheme is preserved", func(t *testing.T) {
		l, err := parseListener("ts+https://:443")
		if err != nil {
			t.Fatalf("parseListener failed: %v", err)
		}
		if l.scheme != "ts+https" {
			t.Errorf("scheme = %q, want ts+https", l.scheme)
		}
	})
}

func TestValidateListeners(t *testing.T) {
	httpL := listener{scheme: "http", port: "8080"}
	tsHTTPS := listener{scheme: "ts+https", port: "443"}
	tsHTTP := listener{scheme: "ts+http", port: "80"}

	t.Run("ts without ts-hostname", func(t *testing.T) {
		old := *tsHostname
		*tsHostname = ""
		defer func() { *tsHostname = old }()
		err := validateListeners([]listener{tsHTTPS})
		if err == nil {
			t.Error("expected error for ts+https without ts-hostname")
		}
	})

	t.Run("ts with ts-hostname", func(t *testing.T) {
		old := *tsHostname
		*tsHostname = "go"
		defer func() { *tsHostname = old }()
		err := validateListeners([]listener{tsHTTPS})
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("multiple ts listeners", func(t *testing.T) {
		old := *tsHostname
		*tsHostname = "go"
		defer func() { *tsHostname = old }()
		err := validateListeners([]listener{tsHTTPS, tsHTTP})
		if err != nil {
			t.Errorf("unexpected error for multiple ts listeners: %v", err)
		}
	})

	t.Run("http only", func(t *testing.T) {
		err := validateListeners([]listener{httpL})
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("empty list", func(t *testing.T) {
		err := validateListeners(nil)
		if err == nil {
			t.Error("expected error for empty listener list")
		}
	})
}

func TestValidateTSHostname(t *testing.T) {
	valid := []string{"go", "golinx", "my-host", "a", "abc123", "a-b-c"}
	for _, name := range valid {
		t.Run("valid/"+name, func(t *testing.T) {
			if err := validateTSHostname(name); err != nil {
				t.Errorf("validateTSHostname(%q) failed: %v", name, err)
			}
		})
	}

	invalid := []string{"", "  ", "-start", "end-", "has space", "has.dot", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
	for _, name := range invalid {
		t.Run("invalid/"+name, func(t *testing.T) {
			if err := validateTSHostname(name); err == nil {
				t.Errorf("validateTSHostname(%q) succeeded, want error", name)
			}
		})
	}
}

// ── TCP Ping ─────────────────────────────────────────────────────

func TestValidatePingHost(t *testing.T) {
	valid := []struct {
		name, host string
	}{
		{"hostname", "google.com"},
		{"host:port", "google.com:443"},
		{"ipv4", "8.8.8.8"},
		{"ipv4:port", "8.8.8.8:53"},
		{"private ip", "192.168.1.1"},
		{"loopback", "127.0.0.1"},
		{"subdomain", "foo.bar.example.com"},
		{"single label", "myhost"},
	}
	for _, tc := range valid {
		t.Run("valid/"+tc.name, func(t *testing.T) {
			if err := validatePingHost(tc.host); err != nil {
				t.Errorf("validatePingHost(%q) failed: %v", tc.host, err)
			}
		})
	}

	invalid := []struct {
		name, host string
	}{
		{"empty", ""},
		{"too long", strings.Repeat("a", 254)},
		{"bad chars", "host name!"},
		{"port zero", "google.com:0"},
		{"port too high", "google.com:99999"},
		{"port non-numeric", "google.com:abc"},
	}
	for _, tc := range invalid {
		t.Run("invalid/"+tc.name, func(t *testing.T) {
			if err := validatePingHost(tc.host); err == nil {
				t.Errorf("validatePingHost(%q) succeeded, want error", tc.host)
			}
		})
	}
}

func TestParsePingTarget(t *testing.T) {
	tests := []struct {
		input, wantHost, wantPort string
	}{
		{"google.com", "google.com", "80"},
		{"google.com:443", "google.com", "443"},
		{"8.8.8.8", "8.8.8.8", "80"},
		{"8.8.8.8:53", "8.8.8.8", "53"},
		{"myhost", "myhost", "80"},
		{"myhost:8443", "myhost", "8443"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			h, p := parsePingTarget(tc.input)
			if h != tc.wantHost || p != tc.wantPort {
				t.Errorf("parsePingTarget(%q) = (%q, %q), want (%q, %q)",
					tc.input, h, p, tc.wantHost, tc.wantPort)
			}
		})
	}
}

func TestServe_PingPage(t *testing.T) {
	mux := serveHandler()

	t.Run("serves HTML page", func(t *testing.T) {
		w := doRequest(t, mux, "GET", "/.ping/google.com")
		if w.Code != 200 {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		ct := w.Header().Get("Content-Type")
		if !strings.Contains(ct, "text/html") {
			t.Errorf("Content-Type = %q, want text/html", ct)
		}
		body := w.Body.String()
		if !strings.Contains(body, "google.com") {
			t.Error("page should contain the host")
		}
		if !strings.Contains(body, "EventSource") {
			t.Error("page should contain EventSource JavaScript")
		}
	})

	t.Run("host with port", func(t *testing.T) {
		w := doRequest(t, mux, "GET", "/.ping/google.com:443")
		if w.Code != 200 {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if !strings.Contains(w.Body.String(), "google.com:443") {
			t.Error("page should show host:port")
		}
	})

	t.Run("rejects empty host", func(t *testing.T) {
		w := doRequest(t, mux, "GET", "/.ping/")
		// /.ping/ with empty host won't match {host}, falls through to serveRedirect
		if w.Code == 200 {
			t.Error("expected non-200 for empty host ping")
		}
	})

	t.Run("rejects bad hostname", func(t *testing.T) {
		w := doRequest(t, mux, "GET", "/.ping/host%20name!")
		if w.Code != 400 {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})
}

// flushRecorder wraps httptest.ResponseRecorder to implement http.Flusher.
type flushRecorder struct {
	*httptest.ResponseRecorder
}

func (f *flushRecorder) Flush() {}

func TestServe_PingSSE(t *testing.T) {
	mux := serveHandler()

	t.Run("SSE content type and events", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/.ping/localhost:1?stream=1", nil)
		w := &flushRecorder{httptest.NewRecorder()}
		mux.ServeHTTP(w, req)

		ct := w.Header().Get("Content-Type")
		if !strings.Contains(ct, "text/event-stream") {
			t.Errorf("Content-Type = %q, want text/event-stream", ct)
		}
		body := w.Body.String()
		if !strings.Contains(body, "event: status") {
			t.Error("SSE stream should contain status event")
		}
		if !strings.Contains(body, "event: info") {
			t.Error("SSE stream should contain info events")
		}
		if !strings.Contains(body, "event: summary") {
			t.Error("SSE stream should contain summary event")
		}
		if !strings.Contains(body, "event: done") {
			t.Error("SSE stream should contain done event")
		}
	})
}

func TestServe_WhoAmIPage(t *testing.T) {
	mux := serveHandler()

	t.Run("serves HTML page", func(t *testing.T) {
		w := doRequest(t, mux, "GET", "/.whoami")
		if w.Code != 200 {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		ct := w.Header().Get("Content-Type")
		if !strings.Contains(ct, "text/html") {
			t.Errorf("Content-Type = %q, want text/html", ct)
		}
		body := w.Body.String()
		if !strings.Contains(body, "Who Am I") {
			t.Error("page should contain title")
		}
		if !strings.Contains(body, "EventSource") {
			t.Error("page should contain EventSource JavaScript")
		}
	})

	t.Run("SSE local mode fallback", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/.whoami?stream=1", nil)
		w := &flushRecorder{httptest.NewRecorder()}
		mux.ServeHTTP(w, req)

		ct := w.Header().Get("Content-Type")
		if !strings.Contains(ct, "text/event-stream") {
			t.Errorf("Content-Type = %q, want text/event-stream", ct)
		}
		body := w.Body.String()
		if !strings.Contains(body, "event: status") {
			t.Error("SSE stream should contain status event")
		}
		if !strings.Contains(body, "local mode") {
			t.Error("SSE stream should indicate local mode when no Tailscale client")
		}
		if !strings.Contains(body, "event: done") {
			t.Error("SSE stream should contain done event")
		}
	})
}

// ---------------------------------------------------------------------------
// Tags
// ---------------------------------------------------------------------------

func TestTags_CreateAndLoad(t *testing.T) {
	mux := serveHandler()
	resetDB(t)

	w := doJSON(t, mux, "POST", "/api/linx", map[string]string{
		"type": "link", "shortName": "tagged", "destinationURL": "https://example.com",
		"tags": "ops, infra",
	})
	if w.Code != 201 {
		t.Fatalf("status = %d, want 201; body: %s", w.Code, w.Body.String())
	}
	var lnx Linx
	json.Unmarshal(w.Body.Bytes(), &lnx)
	if lnx.Tags != "ops, infra" {
		t.Errorf("tags = %q, want %q", lnx.Tags, "ops, infra")
	}

	// Verify round-trip through LoadByShortName
	loaded, err := db.LoadByShortName("tagged")
	if err != nil {
		t.Fatalf("LoadByShortName: %v", err)
	}
	if loaded.Tags != "ops, infra" {
		t.Errorf("loaded tags = %q, want %q", loaded.Tags, "ops, infra")
	}
}

func TestTags_Update(t *testing.T) {
	mux := serveHandler()
	resetDB(t)

	// Create
	w := doJSON(t, mux, "POST", "/api/linx", map[string]string{
		"type": "link", "shortName": "tagupd", "destinationURL": "https://example.com",
		"tags": "old-tag",
	})
	if w.Code != 201 {
		t.Fatalf("create status = %d", w.Code)
	}
	var created Linx
	json.Unmarshal(w.Body.Bytes(), &created)

	// Update tags
	w = doJSON(t, mux, "PUT", "/api/linx/"+strconv.FormatInt(created.ID, 10), map[string]string{
		"type": "link", "shortName": "tagupd", "destinationURL": "https://example.com",
		"tags": "new-tag, extra",
	})
	if w.Code != 200 {
		t.Fatalf("update status = %d; body: %s", w.Code, w.Body.String())
	}
	var updated Linx
	json.Unmarshal(w.Body.Bytes(), &updated)
	if updated.Tags != "new-tag, extra" {
		t.Errorf("updated tags = %q, want %q", updated.Tags, "new-tag, extra")
	}
}

func TestTags_EmptyByDefault(t *testing.T) {
	mux := serveHandler()
	resetDB(t)

	w := doJSON(t, mux, "POST", "/api/linx", map[string]string{
		"type": "link", "shortName": "notag", "destinationURL": "https://example.com",
	})
	if w.Code != 201 {
		t.Fatalf("status = %d", w.Code)
	}
	var lnx Linx
	json.Unmarshal(w.Body.Bytes(), &lnx)
	if lnx.Tags != "" {
		t.Errorf("tags = %q, want empty", lnx.Tags)
	}
}

func TestNormalizeTags(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty", "", ""},
		{"single", "ops", "ops"},
		{"multiple", "ops, infra, dev", "ops, infra, dev"},
		{"uppercase to lower", "Ops, INFRA", "ops, infra"},
		{"spaces become dashes", "my project, hello world", "my-project, hello-world"},
		{"extra whitespace", "  ops  ,  infra  ", "ops, infra"},
		{"dedup", "ops, infra, ops", "ops, infra"},
		{"dedup case insensitive", "Ops, ops", "ops"},
		{"max 10 tags", "a,b,c,d,e,f,g,h,i,j,k,l", "a, b, c, d, e, f, g, h, i, j"},
		{"tag max 30 chars", "abcdefghijklmnopqrstuvwxyz12345678", "abcdefghijklmnopqrstuvwxyz1234"},
		{"blank tags removed", "ops,,, infra,, ", "ops, infra"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeTags(tt.input)
			if got != tt.want {
				t.Errorf("normalizeTags(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// OpenSearch
// ---------------------------------------------------------------------------

func TestOpenSearchXML(t *testing.T) {
	mux := serveHandler()
	req := httptest.NewRequest("GET", "/opensearch.xml", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "opensearchdescription+xml") {
		t.Fatalf("Content-Type = %q, want opensearchdescription+xml", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/api/suggest") {
		t.Fatalf("body missing /api/suggest URL")
	}
	if !strings.Contains(body, "{searchTerms}") {
		t.Fatalf("body missing {searchTerms} template")
	}
}

func TestAPISuggest(t *testing.T) {
	resetDB(t)
	seedTestData(t)
	mux := serveHandler()

	// Match on short name substring
	req := httptest.NewRequest("GET", "/api/suggest?q=goo", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var result []json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(result) != 4 {
		t.Fatalf("got %d elements, want 4", len(result))
	}

	var query string
	json.Unmarshal(result[0], &query)
	if query != "goo" {
		t.Fatalf("query = %q, want %q", query, "goo")
	}

	var names []string
	json.Unmarshal(result[1], &names)
	if len(names) == 0 {
		t.Fatal("expected at least one suggestion for 'goo'")
	}
	found := false
	for _, n := range names {
		if n == "google" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected 'google' in suggestions, got %v", names)
	}

	// Empty query returns empty arrays
	req = httptest.NewRequest("GET", "/api/suggest?q=", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("empty query status = %d, want 200", rec.Code)
	}

	// Person type shows name in description
	req = httptest.NewRequest("GET", "/api/suggest?q=john", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	json.Unmarshal(rec.Body.Bytes(), &result)
	var descs []string
	json.Unmarshal(result[2], &descs)
	if len(descs) == 0 {
		t.Fatal("expected suggestion for 'john'")
	}
	if !strings.Contains(descs[0], "John") {
		t.Fatalf("person description = %q, want to contain 'John'", descs[0])
	}
}

// ---------------------------------------------------------------------------
// Soft Delete / Restore
// ---------------------------------------------------------------------------

func TestDB_LoadAll_ExcludesDeleted(t *testing.T) {
	resetDB(t)
	seedTestData(t)

	all, _ := db.LoadAll("")
	count := len(all)

	lnx, _ := db.LoadByShortName("github")
	db.Delete(lnx.ID)

	all, _ = db.LoadAll("")
	if len(all) != count-1 {
		t.Errorf("LoadAll after delete = %d, want %d", len(all), count-1)
	}
}

func TestAPI_LinxRestore(t *testing.T) {
	mux := serveHandler()

	t.Run("valid restore", func(t *testing.T) {
		resetDB(t)
		ids := seedTestData(t)
		id := ids["google"]
		db.Delete(id)
		w := doRequest(t, mux, "POST", fmt.Sprintf("/api/linx/%d/restore", id))
		if w.Code != 200 {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
	})

	t.Run("restore non-deleted", func(t *testing.T) {
		resetDB(t)
		ids := seedTestData(t)
		id := ids["google"]
		w := doRequest(t, mux, "POST", fmt.Sprintf("/api/linx/%d/restore", id))
		if w.Code != 400 {
			t.Errorf("status = %d, want 400", w.Code)
		}
	})

	t.Run("not found", func(t *testing.T) {
		resetDB(t)
		w := doRequest(t, mux, "POST", "/api/linx/999999/restore")
		if w.Code != 404 {
			t.Errorf("status = %d, want 404", w.Code)
		}
	})
}

func TestDeletedPage(t *testing.T) {
	resetDB(t)
	seedTestData(t)
	mux := serveHandler()

	lnx, _ := db.LoadByShortName("github")
	db.Delete(lnx.ID)

	w := doRequest(t, mux, "GET", "/.deleted")
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "github") {
		t.Error("deleted page should contain 'github'")
	}
	if !strings.Contains(body, "Undelete") {
		t.Error("deleted page should contain 'Undelete' button")
	}
}

// ---------------------------------------------------------------------------
// Document type tests
// ---------------------------------------------------------------------------

func TestDocumentCreate(t *testing.T) {
	resetDB(t)
	mux := serveHandler()

	// Create a document linx.
	w := doJSON(t, mux, "POST", "/api/linx", map[string]string{
		"type": "document", "shortName": "doc1", "description": "Test Doc",
	})
	if w.Code != 201 {
		t.Fatalf("status = %d, want 201; body: %s", w.Code, w.Body.String())
	}
	var lnx Linx
	json.Unmarshal(w.Body.Bytes(), &lnx)
	if lnx.Type != LinxTypeDocument {
		t.Errorf("type = %q, want %q", lnx.Type, LinxTypeDocument)
	}

	// Upload content.
	w = doJSON(t, mux, "POST", fmt.Sprintf("/api/linx/%d/document", lnx.ID), map[string]string{
		"content": "# Hello World", "mime": "text/markdown",
	})
	if w.Code != 200 {
		t.Fatalf("upload status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
}

func TestDocumentRead(t *testing.T) {
	resetDB(t)
	mux := serveHandler()

	// Create and upload.
	w := doJSON(t, mux, "POST", "/api/linx", map[string]string{
		"type": "document", "shortName": "doc2", "description": "Read Test",
	})
	var lnx Linx
	json.Unmarshal(w.Body.Bytes(), &lnx)

	content := "Some **bold** text"
	doJSON(t, mux, "POST", fmt.Sprintf("/api/linx/%d/document", lnx.ID), map[string]string{
		"content": content, "mime": "text/markdown",
	})

	// Read back raw content.
	w = doRequest(t, mux, "GET", fmt.Sprintf("/api/linx/%d/document", lnx.ID))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != content {
		t.Errorf("content = %q, want %q", got, content)
	}
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("Content-Type = %q, want text/markdown", ct)
	}
}

func TestDocumentRendering(t *testing.T) {
	resetDB(t)
	mux := serveHandler()

	// Create and upload markdown.
	w := doJSON(t, mux, "POST", "/api/linx", map[string]string{
		"type": "document", "shortName": "mddoc", "description": "Markdown Doc",
	})
	var lnx Linx
	json.Unmarshal(w.Body.Bytes(), &lnx)

	doJSON(t, mux, "POST", fmt.Sprintf("/api/linx/%d/document", lnx.ID), map[string]string{
		"content": "# Hello", "mime": "text/markdown",
	})

	// Access the reader page.
	w = doRequest(t, mux, "GET", "/mddoc")
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "<h1>Hello</h1>") {
		t.Error("rendered page should contain <h1>Hello</h1>")
	}
	if !strings.Contains(body, "Markdown Doc") {
		t.Error("rendered page should contain document title")
	}
}

func TestDocumentHTMLSanitization(t *testing.T) {
	resetDB(t)
	mux := serveHandler()

	w := doJSON(t, mux, "POST", "/api/linx", map[string]string{
		"type": "document", "shortName": "xssdoc", "description": "XSS Test",
	})
	var lnx Linx
	json.Unmarshal(w.Body.Bytes(), &lnx)

	doJSON(t, mux, "POST", fmt.Sprintf("/api/linx/%d/document", lnx.ID), map[string]string{
		"content": "<p>Safe</p><script>alert(1)</script>", "mime": "text/html",
	})

	w = doRequest(t, mux, "GET", "/xssdoc")
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "<script>") {
		t.Error("rendered page should NOT contain <script> tags")
	}
	if !strings.Contains(body, "Safe") {
		t.Error("rendered page should contain safe content")
	}
}

func TestDocumentValidation(t *testing.T) {
	resetDB(t)
	mux := serveHandler()

	// Missing description should fail.
	w := doJSON(t, mux, "POST", "/api/linx", map[string]string{
		"type": "document", "shortName": "nodesc",
	})
	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestDocumentUploadInvalidMime(t *testing.T) {
	resetDB(t)
	mux := serveHandler()

	w := doJSON(t, mux, "POST", "/api/linx", map[string]string{
		"type": "document", "shortName": "badmime", "description": "Bad Mime",
	})
	var lnx Linx
	json.Unmarshal(w.Body.Bytes(), &lnx)

	// Invalid mime type defaults to text/markdown.
	doJSON(t, mux, "POST", fmt.Sprintf("/api/linx/%d/document", lnx.ID), map[string]string{
		"content": "hello", "mime": "application/pdf",
	})

	w = doRequest(t, mux, "GET", fmt.Sprintf("/api/linx/%d/document", lnx.ID))
	ct := w.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("Content-Type = %q, want text/markdown (default for invalid mime)", ct)
	}
}

func TestDocumentPlainText(t *testing.T) {
	resetDB(t)
	mux := serveHandler()

	w := doJSON(t, mux, "POST", "/api/linx", map[string]string{
		"type": "document", "shortName": "txtdoc", "description": "Plain Text",
	})
	var lnx Linx
	json.Unmarshal(w.Body.Bytes(), &lnx)

	doJSON(t, mux, "POST", fmt.Sprintf("/api/linx/%d/document", lnx.ID), map[string]string{
		"content": "Hello <world> & friends", "mime": "text/plain",
	})

	w = doRequest(t, mux, "GET", "/txtdoc")
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	// Plain text should be HTML-escaped in a <pre> block.
	if !strings.Contains(body, "&lt;world&gt;") {
		t.Error("plain text should be HTML-escaped")
	}
}

// ---------------------------------------------------------------------------
// Benchmarks — concurrent readers & writers on WAL-mode SQLite
// ---------------------------------------------------------------------------

// benchDB creates an isolated on-disk WAL-mode database seeded with n links.
func benchDB(b *testing.B, n int) *SQLiteDB {
	b.Helper()
	dir := b.TempDir()
	d, err := NewSQLiteDB(dir + "/bench.db")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { d.db.Close() })
	for i := 0; i < n; i++ {
		d.insertLinxUnlocked(&Linx{
			Type:           LinxTypeLink,
			ShortName:      fmt.Sprintf("link%d", i),
			DestinationURL: fmt.Sprintf("https://example.com/%d", i),
			Description:    fmt.Sprintf("Benchmark link %d", i),
			Owner:          "bench@test",
		})
	}
	return d
}

// insertLinxUnlocked inserts without locking — only for benchmark seeding.
func (s *SQLiteDB) insertLinxUnlocked(lnx *Linx) (int64, error) {
	if lnx.Type == "" {
		lnx.Type = LinxTypeLink
	}
	now := time.Now().Unix()
	result, err := s.db.Exec(
		`INSERT INTO Linx (Type, ShortName, DestinationURL, Description, Owner, FirstName, LastName, Title, Email, Phone, WebLink, CalLink, XLink, LinkedInLink, Color, Tags, DateCreated) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		lnx.Type, lnx.ShortName, lnx.DestinationURL, lnx.Description, lnx.Owner,
		lnx.FirstName, lnx.LastName, lnx.Title, lnx.Email, lnx.Phone,
		lnx.WebLink, lnx.CalLink, lnx.XLink, lnx.LinkedInLink, lnx.Color, lnx.Tags, now,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func BenchmarkReadOnly_LoadByShortName(b *testing.B) {
	d := benchDB(b, 1000)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			name := fmt.Sprintf("link%d", i%1000)
			d.LoadByShortName(name)
			i++
		}
	})
}

func BenchmarkReadOnly_LoadAll(b *testing.B) {
	d := benchDB(b, 500)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			d.LoadAll("")
		}
	})
}

func BenchmarkReadOnly_Suggest(b *testing.B) {
	d := benchDB(b, 1000)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			d.Suggest(fmt.Sprintf("link%d", i%100), 8)
			i++
		}
	})
}

func BenchmarkWriteOnly_IncrementClick(b *testing.B) {
	d := benchDB(b, 1000)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			d.IncrementClick(fmt.Sprintf("link%d", i%1000))
			i++
		}
	})
}

func BenchmarkWriteOnly_Save(b *testing.B) {
	d := benchDB(b, 0)
	b.ResetTimer()
	// Serial — each Save creates a unique short name.
	for i := 0; i < b.N; i++ {
		d.Save(&Linx{
			Type:           LinxTypeLink,
			ShortName:      fmt.Sprintf("bench%d", i),
			DestinationURL: "https://example.com",
			Owner:          "bench@test",
		})
	}
}

// BenchmarkConcurrent_ReadsAndWrites simulates a realistic workload:
// many concurrent readers (LoadByShortName, Suggest) with a steady
// stream of writers (IncrementClick). Reports combined throughput.
func BenchmarkConcurrent_ReadsAndWrites(b *testing.B) {
	d := benchDB(b, 1000)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			idx := i % 1000
			switch i % 10 {
			case 0: // 10% writes — click tracking
				d.IncrementClick(fmt.Sprintf("link%d", idx))
			case 1: // 10% reads — suggest (heavier query)
				d.Suggest(fmt.Sprintf("link%d", idx%100), 8)
			default: // 80% reads — short name lookup (redirect path)
				d.LoadByShortName(fmt.Sprintf("link%d", idx))
			}
			i++
		}
	})
}

// BenchmarkConcurrent_HeavyWrite tests a write-heavy mix (50/50).
func BenchmarkConcurrent_HeavyWrite(b *testing.B) {
	d := benchDB(b, 1000)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			idx := i % 1000
			if i%2 == 0 {
				d.IncrementClick(fmt.Sprintf("link%d", idx))
			} else {
				d.LoadByShortName(fmt.Sprintf("link%d", idx))
			}
			i++
		}
	})
}
