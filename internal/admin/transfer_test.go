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
