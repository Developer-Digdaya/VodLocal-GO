package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
	"time"
)

// decodeTarget is what the GetJSON tests decode into.
type decodeTarget struct {
	Code  string `json:"code"`
	Items []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"items"`
}

// newJSONServer starts a test server that always answers with body and status.
func newJSONServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if _, err := w.Write([]byte(body)); err != nil {
			t.Logf("menulis body: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Test 4: a successful response is decoded into out.
func TestGetJSONSuccess(t *testing.T) {
	const body = `{"code":"ok","items":[{"id":"mv-1","name":"Hujan"},{"id":"sr-1","name":"Rahasia"}]}`
	srv := newJSONServer(t, http.StatusOK, body)

	c := NewClient(srv.URL, 2*time.Second)

	var got decodeTarget
	if err := c.GetJSON(context.Background(), "/v1/catalog", nil, &got); err != nil {
		t.Fatalf("GetJSON() error = %v", err)
	}
	if got.Code != "ok" {
		t.Errorf("Code = %q, want %q", got.Code, "ok")
	}
	if len(got.Items) != 2 {
		t.Fatalf("len(Items) = %d, want 2", len(got.Items))
	}
	if got.Items[0].ID != "mv-1" || got.Items[1].Name != "Rahasia" {
		t.Errorf("Items = %+v, want the payload verbatim", got.Items)
	}

	// A nil out only checks that the call succeeded.
	if err := c.GetJSON(context.Background(), "/v1/catalog", nil, nil); err != nil {
		t.Errorf("GetJSON(nil out) error = %v", err)
	}
}

// The request must be a GET carrying the configured headers.
func TestGetJSONRequestShape(t *testing.T) {
	var method, accept, apiKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, accept, apiKey = r.Method, r.Header.Get("Accept"), r.Header.Get("X-Api-Key")
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, 2*time.Second)
	c.Header = http.Header{"X-Api-Key": {"rahasia"}}

	if err := c.GetJSON(context.Background(), "/v1/catalog", nil, nil); err != nil {
		t.Fatalf("GetJSON() error = %v", err)
	}
	if method != http.MethodGet {
		t.Errorf("method = %q, want GET", method)
	}
	if accept != "application/json" {
		t.Errorf("Accept = %q, want application/json", accept)
	}
	if apiKey != "rahasia" {
		t.Errorf("X-Api-Key = %q, want %q", apiKey, "rahasia")
	}
}

// Test 5: 404 becomes ErrNotFound.
func TestGetJSONNotFound(t *testing.T) {
	srv := newJSONServer(t, http.StatusNotFound, `{"error":"tidak ada"}`)
	c := NewClient(srv.URL, 2*time.Second)

	err := c.GetJSON(context.Background(), "/v1/titles/hilang", nil, &decodeTarget{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetJSON() error = %v, want ErrNotFound", err)
	}
	if errors.Is(err, ErrUpstream) || errors.Is(err, ErrBadResponse) || errors.Is(err, ErrUpstreamTimeout) {
		t.Errorf("GetJSON() error = %v, want it to match ErrNotFound only", err)
	}
}

// Test 6: a 5xx becomes ErrUpstream, while 408 and 504 become
// ErrUpstreamTimeout.
func TestGetJSONServerErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   error
	}{
		{"500", http.StatusInternalServerError, ErrUpstream},
		{"502", http.StatusBadGateway, ErrUpstream},
		{"503", http.StatusServiceUnavailable, ErrUpstream},
		{"408", http.StatusRequestTimeout, ErrUpstreamTimeout},
		{"504", http.StatusGatewayTimeout, ErrUpstreamTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newJSONServer(t, tc.status, `{"error":"gagal"}`)
			c := NewClient(srv.URL, 2*time.Second)

			err := c.GetJSON(context.Background(), "/v1/titles", nil, &decodeTarget{})
			if !errors.Is(err, tc.want) {
				t.Fatalf("GetJSON() error = %v, want %v", err, tc.want)
			}
			if tc.want == ErrUpstream && errors.Is(err, ErrUpstreamTimeout) {
				t.Errorf("GetJSON() error = %v, want it not to match ErrUpstreamTimeout", err)
			}
		})
	}
}

// Test 7: broken JSON becomes ErrBadResponse.
func TestGetJSONBadJSON(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"terpotong", `{"code":"ok","items":[`},
		{"bukan json", `<html>oops</html>`},
		{"kosong", ``},
		{"tipe salah", `{"code":42}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newJSONServer(t, http.StatusOK, tc.body)
			c := NewClient(srv.URL, 2*time.Second)

			err := c.GetJSON(context.Background(), "/v1/titles", nil, &decodeTarget{})
			if !errors.Is(err, ErrBadResponse) {
				t.Fatalf("GetJSON() error = %v, want ErrBadResponse", err)
			}
		})
	}
}

// A body over the 4 MB cap is refused instead of being buffered.
func TestGetJSONBodyTooLarge(t *testing.T) {
	big := make([]byte, DefaultMaxResponseBytes+16)
	for i := range big {
		big[i] = 'a'
	}
	srv := newJSONServer(t, http.StatusOK, string(big))
	c := NewClient(srv.URL, 5*time.Second)

	err := c.GetJSON(context.Background(), "/v1/titles", nil, &decodeTarget{})
	if !errors.Is(err, ErrBadResponse) {
		t.Fatalf("GetJSON() error = %v, want ErrBadResponse", err)
	}
}

// Test 8: a slow server plus a short client timeout becomes ErrUpstreamTimeout,
// without sleeping for real.
func TestGetJSONTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The client gives up after 50 ms and cancels the request, which ends
		// this wait immediately; the timer only keeps the handler from
		// leaking if cancellation ever fails to arrive.
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	c := NewClient(srv.URL, 50*time.Millisecond)

	start := time.Now()
	err := c.GetJSON(context.Background(), "/v1/titles", nil, &decodeTarget{})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrUpstreamTimeout) {
		t.Fatalf("GetJSON() error = %v, want ErrUpstreamTimeout", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("GetJSON() took %v, want about the 50 ms timeout", elapsed)
	}
	// The underlying cause stays reachable.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("GetJSON() error = %v, want it to wrap context.DeadlineExceeded", err)
	}
}

// A cancelled context is reported as cancellation, not as an upstream problem.
func TestGetJSONContextCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, 5*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.GetJSON(ctx, "/v1/titles", nil, &decodeTarget{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("GetJSON() error = %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrUpstream) || errors.Is(err, ErrUpstreamTimeout) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrBadResponse) {
		t.Errorf("GetJSON() error = %v, want it not to match any provider sentinel", err)
	}
}

// An unreachable service becomes ErrUpstream.
func TestGetJSONUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := srv.URL
	srv.Close() // nothing listens on addr any more

	c := NewClient(addr, time.Second)

	err := c.GetJSON(context.Background(), "/v1/titles", nil, &decodeTarget{})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("GetJSON() error = %v, want ErrUpstream", err)
	}
}

// Test 9: path and query reach the server exactly as the caller built them.
func TestGetJSONPathAndQuery(t *testing.T) {
	type seen struct {
		path    string
		raw     string
		values  url.Values
		request string
	}
	var got seen

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = seen{
			path:    r.URL.Path,
			raw:     r.URL.RawQuery,
			values:  r.URL.Query(),
			request: r.RequestURI,
		}
		w.Write([]byte(`{"code":"ok"}`))
	}))
	t.Cleanup(srv.Close)

	// A trailing slash on the base URL and a leading slash on the path must not
	// produce a doubled slash.
	c := NewClient(srv.URL+"/", 2*time.Second)
	query := url.Values{"page": {"2"}, "genre": {"drama"}}

	var out decodeTarget
	if err := c.GetJSON(context.Background(), "/v1/titles", query, &out); err != nil {
		t.Fatalf("GetJSON() error = %v", err)
	}

	if got.path != "/v1/titles" {
		t.Errorf("path = %q, want %q", got.path, "/v1/titles")
	}
	if got.raw != "genre=drama&page=2" {
		t.Errorf("RawQuery = %q, want %q", got.raw, "genre=drama&page=2")
	}
	if got.request != "/v1/titles?genre=drama&page=2" {
		t.Errorf("RequestURI = %q, want %q", got.request, "/v1/titles?genre=drama&page=2")
	}
	if !reflect.DeepEqual(got.values, query) {
		t.Errorf("query = %v, want %v", got.values, query)
	}
	if out.Code != "ok" {
		t.Errorf("Code = %q, want %q", out.Code, "ok")
	}

	// Without query values no "?" is appended.
	got = seen{}
	if err := c.GetJSON(context.Background(), "v1/titles", nil, &out); err != nil {
		t.Fatalf("GetJSON() error = %v", err)
	}
	if got.raw != "" {
		t.Errorf("RawQuery = %q, want empty", got.raw)
	}
	if got.path != "/v1/titles" {
		t.Errorf("path = %q, want %q", got.path, "/v1/titles")
	}
}
