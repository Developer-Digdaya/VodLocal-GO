package admin_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestSyncShowsTransferControls(t *testing.T) {
	f := newFixture(t)
	f.login()

	rec := f.get("/sync")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, mau 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Export ke HDD",
		"Offline (HDD)",
		"id=\"content-source\"",
		"/transfer/drives",
		"/transfer/local-content",
		"/transfer/import",
		"/transfer/export",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("halaman sync tidak memuat %q", want)
		}
	}
}

func TestTransferLocalContentPagination(t *testing.T) {
	f := newFixture(t)
	f.login()

	rec := f.do(http.MethodGet, "/transfer/local-content?page=2&per_page=30", nil, "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Data struct {
			Items []json.RawMessage `json:"items"`
			Meta  struct {
				Page       int   `json:"page"`
				PerPage    int   `json:"per_page"`
				Total      int64 `json:"total"`
				TotalPages int64 `json:"total_pages"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Data.Meta.Page != 2 || body.Data.Meta.PerPage != 30 || body.Data.Meta.Total != 0 {
		t.Fatalf("meta = %+v", body.Data.Meta)
	}
	if body.Data.Items == nil {
		t.Fatal("items bernilai null, mau array kosong")
	}
}

func TestTransferJSONEndpoints(t *testing.T) {
	f := newFixture(t)
	f.login()

	for _, path := range []string{"/transfer/drives", "/transfer/local-content"} {
		rec := f.do(http.MethodGet, path, nil, "application/json")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d (body=%s)", path, rec.Code, rec.Body.String())
		}
		var env struct {
			Code int             `json:"code"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s: JSON tidak valid: %v", path, err)
		}
		if env.Code != 0 || len(env.Data) == 0 {
			t.Fatalf("%s: envelope = %+v", path, env)
		}
	}
}
