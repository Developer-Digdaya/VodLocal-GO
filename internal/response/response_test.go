package response

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// allCodes memuat seluruh konstanta kode yang didefinisikan paket ini, beserta
// nama konstanta untuk pesan kegagalan test.
func allCodes() []struct {
	name string
	code int
} {
	return []struct {
		name string
		code int
	}{
		{"CodeSuccess", CodeSuccess},
		{"CodePartialFailure", CodePartialFailure},

		{"CodeInternalError", CodeInternalError},
		{"CodeBadRequest", CodeBadRequest},
		{"CodeNotFound", CodeNotFound},
		{"CodeValidationError", CodeValidationError},
		{"CodeMethodNotAllowed", CodeMethodNotAllowed},
		{"CodePayloadTooLarge", CodePayloadTooLarge},
		{"CodeUnsupportedMediaType", CodeUnsupportedMediaType},
		{"CodeRateLimited", CodeRateLimited},
		{"CodeServiceUnavailable", CodeServiceUnavailable},
		{"CodeRequestTimeout", CodeRequestTimeout},
		{"CodeDatabaseError", CodeDatabaseError},

		{"CodeUnauthorized", CodeUnauthorized},
		{"CodeForbidden", CodeForbidden},
		{"CodeDeviceNotFound", CodeDeviceNotFound},
		{"CodeDeviceAlreadyRegistered", CodeDeviceAlreadyRegistered},
		{"CodeDeviceDisabled", CodeDeviceDisabled},
		{"CodeSessionExpired", CodeSessionExpired},
		{"CodeInvalidCredentials", CodeInvalidCredentials},
		{"CodeCSRFInvalid", CodeCSRFInvalid},
		{"CodeTokenInvalid", CodeTokenInvalid},

		{"CodeTitleNotFound", CodeTitleNotFound},
		{"CodeEpisodeNotFound", CodeEpisodeNotFound},
		{"CodePlayableNotFound", CodePlayableNotFound},
		{"CodePlayableNotReady", CodePlayableNotReady},
		{"CodeGenreNotFound", CodeGenreNotFound},
		{"CodeSearchQueryInvalid", CodeSearchQueryInvalid},
		{"CodeContentNotPublished", CodeContentNotPublished},

		{"CodeSyncNotFound", CodeSyncNotFound},
		{"CodeSyncAlreadyRunning", CodeSyncAlreadyRunning},
		{"CodeSyncNotCancellable", CodeSyncNotCancellable},
		{"CodeProviderError", CodeProviderError},
		{"CodeProviderDisabled", CodeProviderDisabled},
		{"CodePlaylistParseError", CodePlaylistParseError},
		{"CodeSegmentDownloadFailed", CodeSegmentDownloadFailed},
		{"CodeVerificationFailed", CodeVerificationFailed},
		{"CodeGenerationSwitchFailed", CodeGenerationSwitchFailed},
		{"CodeNoActiveGeneration", CodeNoActiveGeneration},

		{"CodeInsufficientStorage", CodeInsufficientStorage},
		{"CodeGenerationActive", CodeGenerationActive},
		{"CodeGenerationNotFound", CodeGenerationNotFound},
		{"CodeGenerationNotDeletable", CodeGenerationNotDeletable},
		{"CodeGenerationMissingOnDisk", CodeGenerationMissingOnDisk},

		{"CodeUpstreamUnavailable", CodeUpstreamUnavailable},
		{"CodeUpstreamTimeout", CodeUpstreamTimeout},
		{"CodeUpstreamAuthFailed", CodeUpstreamAuthFailed},
		{"CodeUpstreamRateLimited", CodeUpstreamRateLimited},
		{"CodeUpstreamBadResponse", CodeUpstreamBadResponse},

		{"CodeMediaNotFound", CodeMediaNotFound},
		{"CodeInvalidMediaPath", CodeInvalidMediaPath},
		{"CodeRangeNotSatisfiable", CodeRangeNotSatisfiable},

		{"CodeProgressNotFound", CodeProgressNotFound},
		{"CodeProgressInvalid", CodeProgressInvalid},
	}
}

// decode merekam response dan mengembalikan recorder serta hasil
// json.Unmarshal ke map[string]any (bentuk yang dilihat klien).
func decode(t *testing.T, fn func(w http.ResponseWriter)) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	rec := httptest.NewRecorder()
	fn(rec)

	if got, want := rec.Header().Get("Content-Type"), "application/json; charset=utf-8"; got != want {
		t.Fatalf("Content-Type = %q, ingin %q", got, want)
	}

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body bukan JSON valid: %v (body=%q)", err, rec.Body.String())
	}
	return rec, out
}

// errorsOf mengambil field errors dari envelope hasil decode.
func errorsOf(t *testing.T, out map[string]any) []any {
	t.Helper()

	raw, ok := out["errors"]
	if !ok {
		t.Fatalf("field errors tidak ada pada %v", out)
	}
	if raw == nil {
		t.Fatalf("errors bernilai null, harus []")
	}
	errs, ok := raw.([]any)
	if !ok {
		t.Fatalf("errors bukan array, dapat %T", raw)
	}
	return errs
}

func TestSuccess(t *testing.T) {
	tests := []struct {
		name string
		call func(w http.ResponseWriter)
		want int
	}{
		{
			name: "dengan data",
			call: func(w http.ResponseWriter) {
				Success(w, map[string]any{"id": 12, "title": "Judul Contoh"})
			},
			want: http.StatusOK,
		},
		{
			name: "tanpa data",
			call: func(w http.ResponseWriter) { Success(w, nil) },
			want: http.StatusOK,
		},
		{
			name: "created",
			call: func(w http.ResponseWriter) {
				Created(w, map[string]any{"id": 12})
			},
			want: http.StatusCreated,
		},
		{
			name: "partial success",
			call: func(w http.ResponseWriter) {
				PartialSuccess(w, map[string]any{"ok": 3}, []Detail{{Field: "a", Message: "gagal"}})
			},
			want: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, out := decode(t, tt.call)

			if rec.Code != tt.want {
				t.Errorf("HTTP status = %d, ingin %d", rec.Code, tt.want)
			}

			wantCode := float64(CodeSuccess)
			if tt.name == "partial success" {
				wantCode = float64(CodePartialFailure)
			}
			if got := out["code"]; got != wantCode {
				t.Errorf("code = %v, ingin %v", got, wantCode)
			}

			wantMsg := Label(CodeSuccess)
			if tt.name == "partial success" {
				wantMsg = Label(CodePartialFailure)
			}
			if got := out["msg"]; got != wantMsg {
				t.Errorf("msg = %v, ingin %q", got, wantMsg)
			}

			errs := errorsOf(t, out)

			switch tt.name {
			case "partial success":
				if len(errs) != 1 {
					t.Errorf("len(errors) = %d, ingin 1", len(errs))
				}
			default:
				if len(errs) != 0 {
					t.Errorf("len(errors) = %d, ingin 0", len(errs))
				}
			}

			if _, ok := out["data"]; !ok {
				t.Errorf("field data tidak ada pada %v", out)
			}
		})
	}
}

func TestError(t *testing.T) {
	tests := []struct {
		name     string
		code     int
		wantHTTP int
	}{
		{"judul tidak ditemukan", CodeTitleNotFound, http.StatusNotFound},
		{"validasi", CodeValidationError, http.StatusUnprocessableEntity},
		{"sync berjalan", CodeSyncAlreadyRunning, http.StatusConflict},
		{"media belum siap", CodePlayableNotReady, http.StatusConflict},
		{"storage penuh", CodeInsufficientStorage, http.StatusConflict},
		{"internal", CodeInternalError, http.StatusInternalServerError},
		{"upstream timeout", CodeUpstreamTimeout, http.StatusGatewayTimeout},
		{"range tidak terpenuhi", CodeRangeNotSatisfiable, http.StatusRequestedRangeNotSatisfiable},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, out := decode(t, func(w http.ResponseWriter) {
				Error(w, tt.code, []Detail{{Field: "id", Message: "tidak valid"}})
			})

			if rec.Code != tt.wantHTTP {
				t.Errorf("HTTP status = %d, ingin %d", rec.Code, tt.wantHTTP)
			}
			if got := out["code"]; got != float64(tt.code) {
				t.Errorf("code = %v, ingin %d", got, tt.code)
			}
			if got, ok := out["data"]; !ok || got != nil {
				t.Errorf("data = %v (hadir=%v), ingin null", got, ok)
			}
			if got := out["msg"]; got != Label(tt.code) {
				t.Errorf("msg = %v, ingin %q", got, Label(tt.code))
			}
			if errs := errorsOf(t, out); len(errs) != 1 {
				t.Errorf("len(errors) = %d, ingin 1", len(errs))
			}
		})
	}
}

func TestErrorNilDetails(t *testing.T) {
	tests := []struct {
		name string
		call func(w http.ResponseWriter)
	}{
		{
			name: "Error dengan nil",
			call: func(w http.ResponseWriter) { Error(w, CodeTitleNotFound, nil) },
		},
		{
			name: "ErrorMsg",
			call: func(w http.ResponseWriter) { ErrorMsg(w, CodeBadRequest, "Permintaan tidak valid") },
		},
		{
			name: "ErrorField",
			call: func(w http.ResponseWriter) { ErrorField(w, CodeValidationError, "body.mac_address", "wajib diisi") },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, out := decode(t, tt.call)

			errs := errorsOf(t, out)
			if errs == nil {
				t.Fatal("errors nil, harus []Detail{}")
			}
		})
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]string
		want   []Detail
	}{
		{
			name:   "dua field terurut",
			fields: map[string]string{"q": "minimal 2 karakter", "per_page": "maksimal 200"},
			want: []Detail{
				{Field: "per_page", Message: "maksimal 200"},
				{Field: "q", Message: "minimal 2 karakter"},
			},
		},
		{
			name:   "banyak field terurut",
			fields: map[string]string{"z": "z", "a": "a", "m": "m", "body.mac_address": "wajib diisi"},
			want: []Detail{
				{Field: "a", Message: "a"},
				{Field: "body.mac_address", Message: "wajib diisi"},
				{Field: "m", Message: "m"},
				{Field: "z", Message: "z"},
			},
		},
		{
			name:   "tanpa field",
			fields: map[string]string{},
			want:   []Detail{},
		},
		{
			name:   "nil map",
			fields: nil,
			want:   []Detail{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, out := decode(t, func(w http.ResponseWriter) {
				ValidationErrors(w, tt.fields)
			})

			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("HTTP status = %d, ingin 422", rec.Code)
			}
			if got := out["code"]; got != float64(CodeValidationError) {
				t.Errorf("code = %v, ingin %d", got, CodeValidationError)
			}
			if got := out["msg"]; got != Label(CodeValidationError) {
				t.Errorf("msg = %v, ingin %q", got, Label(CodeValidationError))
			}
			if got, ok := out["data"]; !ok || got != nil {
				t.Errorf("data = %v (hadir=%v), ingin null", got, ok)
			}

			errs := errorsOf(t, out)
			if len(errs) != len(tt.want) {
				t.Fatalf("len(errors) = %d, ingin %d", len(errs), len(tt.want))
			}
			for i, want := range tt.want {
				got, ok := errs[i].(map[string]any)
				if !ok {
					t.Fatalf("errors[%d] bukan objek, dapat %T", i, errs[i])
				}
				if got["field"] != want.Field {
					t.Errorf("errors[%d].field = %v, ingin %q", i, got["field"], want.Field)
				}
				if got["message"] != want.Message {
					t.Errorf("errors[%d].message = %v, ingin %q", i, got["message"], want.Message)
				}
			}
		})
	}
}

func TestHTTPStatusUnknownCode(t *testing.T) {
	tests := []struct {
		name string
		code int
		want int
	}{
		{"negatif", -1, http.StatusInternalServerError},
		{"nol di luar tabel", 42, http.StatusInternalServerError},
		{"ratusan", 999, http.StatusInternalServerError},
		{"tak terdaftar", 9999, http.StatusInternalServerError},
		{"dalam rentang tapi tak terdaftar", 1020, http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HTTPStatus(tt.code); got != tt.want {
				t.Errorf("HTTPStatus(%d) = %d, ingin %d", tt.code, got, tt.want)
			}
		})
	}
}

func TestHTTPStatusKnownCodes(t *testing.T) {
	tests := []struct {
		name string
		code int
		want int
	}{
		{"sukses", CodeSuccess, http.StatusOK},
		{"partial", CodePartialFailure, http.StatusOK},
		{"validasi", CodeValidationError, http.StatusUnprocessableEntity},
		{"sync berjalan", CodeSyncAlreadyRunning, http.StatusConflict},
		{"unauthorized", CodeUnauthorized, http.StatusUnauthorized},
		{"forbidden", CodeForbidden, http.StatusForbidden},
		{"device sudah terdaftar", CodeDeviceAlreadyRegistered, http.StatusConflict},
		{"playable belum siap", CodePlayableNotReady, http.StatusConflict},
		{"provider error", CodeProviderError, http.StatusBadGateway},
		{"storage penuh", CodeInsufficientStorage, http.StatusConflict},
		{"upstream timeout", CodeUpstreamTimeout, http.StatusGatewayTimeout},
		{"media tidak ada", CodeMediaNotFound, http.StatusNotFound},
		{"range", CodeRangeNotSatisfiable, http.StatusRequestedRangeNotSatisfiable},
		{"progress tidak valid", CodeProgressInvalid, http.StatusUnprocessableEntity},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HTTPStatus(tt.code); got != tt.want {
				t.Errorf("HTTPStatus(%d) = %d, ingin %d", tt.code, got, tt.want)
			}
		})
	}
}

func TestLabel(t *testing.T) {
	t.Run("stabil dan tidak kosong untuk semua konstanta", func(t *testing.T) {
		for _, c := range allCodes() {
			first := Label(c.code)
			if first == "" {
				t.Errorf("%s (%d): Label kosong", c.name, c.code)
			}
			if second := Label(c.code); second != first {
				t.Errorf("%s (%d): Label tidak stabil %q != %q", c.name, c.code, second, first)
			}
			if _, ok := labels[c.code]; !ok {
				t.Errorf("%s (%d): tidak ada entri di labels", c.name, c.code)
			}
			if _, ok := httpStatus[c.code]; !ok {
				t.Errorf("%s (%d): tidak ada entri di httpStatus", c.name, c.code)
			}
		}
	})

	t.Run("label baku sesuai dokumen", func(t *testing.T) {
		tests := []struct {
			code int
			want string
		}{
			{CodeSuccess, "Berhasil"},
			{CodePartialFailure, "Berhasil sebagian"},
			{CodeValidationError, "Data yang dikirim tidak valid"},
			{CodeTitleNotFound, "Judul tidak ditemukan"},
			{CodePlayableNotReady, "Media belum selesai diunduh"},
			{CodeSyncAlreadyRunning, "Masih ada sinkronisasi yang berjalan"},
			{CodeInsufficientStorage, "Ruang penyimpanan tidak mencukupi"},
		}

		for _, tt := range tests {
			if got := Label(tt.code); got != tt.want {
				t.Errorf("Label(%d) = %q, ingin %q", tt.code, got, tt.want)
			}
		}
	})

	t.Run("kode tidak dikenal", func(t *testing.T) {
		tests := []int{-1, 9999, 1020}
		for _, code := range tests {
			got := Label(code)
			if got == "" {
				t.Errorf("Label(%d) kosong", code)
			}
			if got != Label(code) {
				t.Errorf("Label(%d) tidak stabil", code)
			}
		}
	})
}

func TestLabelsUnikTidakBentrok(t *testing.T) {
	seen := make(map[int]string, len(allCodes()))
	for _, c := range allCodes() {
		if prev, ok := seen[c.code]; ok {
			t.Fatalf("kode %d dipakai dua konstanta: %s dan %s", c.code, prev, c.name)
		}
		seen[c.code] = c.name
	}
}

func TestWriteMemaksaErrorsTidakNil(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, http.StatusOK, Body{Code: CodeSuccess, Msg: Label(CodeSuccess), Data: nil, Errors: nil})

	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body bukan JSON valid: %v", err)
	}
	if out["errors"] == nil {
		t.Fatal("errors null, harus []")
	}
}
