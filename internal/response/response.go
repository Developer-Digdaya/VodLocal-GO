// Package response menyediakan envelope JSON standar untuk seluruh handler
// HTTP (net/http + chi) di proyek ini.
//
// Bentuk envelope (docs/standar-response.md §2):
//
//	{ "code": 0, "msg": "Berhasil", "data": null, "errors": [] }
//
// Aturan wajib yang dijaga paket ini:
//   - R1: HTTP status selalu diturunkan dari kode lewat HTTPStatus(code).
//   - R2: msg selalu Label(code), tidak pernah ditulis manual.
//   - R3/R4: data dan errors selalu hadir; errors tidak pernah null.
package response

import (
	"encoding/json"
	"net/http"
	"sort"
)

// contentType adalah satu-satunya Content-Type yang dipakai envelope JSON.
const contentType = "application/json; charset=utf-8"

// Detail menjelaskan satu error: path field dan pesan untuk manusia.
type Detail struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Body adalah bentuk standar JSON response.
type Body struct {
	Code   int      `json:"code"`
	Msg    string   `json:"msg"`
	Data   any      `json:"data"`
	Errors []Detail `json:"errors"`
}

// Label mengembalikan pesan baku (msg) untuk sebuah kode aplikasi.
// Kode yang tidak dikenal memakai pesan kesalahan server generik.
func Label(code int) string {
	if l, ok := labels[code]; ok {
		return l
	}
	return defaultLabel
}

// HTTPStatus menurunkan status HTTP dari kode aplikasi (§5).
// Kode yang tidak dikenal menghasilkan 500 (fail closed).
func HTTPStatus(code int) int {
	if s, ok := httpStatus[code]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// newBody membangun Body dengan msg dari Label(code) dan errors yang tidak
// pernah nil.
func newBody(code int, data any, errs []Detail) Body {
	if errs == nil {
		errs = []Detail{}
	}
	return Body{Code: code, Msg: Label(code), Data: data, Errors: errs}
}

// toDetails mengubah map field→message menjadi []Detail yang terurut
// berdasarkan Field, sehingga output deterministik.
func toDetails(fields map[string]string) []Detail {
	details := make([]Detail, 0, len(fields))
	for field, msg := range fields {
		details = append(details, Detail{Field: field, Message: msg})
	}
	sort.Slice(details, func(i, j int) bool {
		return details[i].Field < details[j].Field
	})
	return details
}

// Write mengirim Body sebagai JSON dengan status HTTP yang diberikan.
// Semua helper lain bermuara di sini supaya header dan encoding seragam.
func Write(w http.ResponseWriter, status int, b Body) {
	if b.Errors == nil {
		b.Errors = []Detail{}
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(b)
}

// Success mengirim response sukses (code 0, HTTP 200) dengan data opsional.
func Success(w http.ResponseWriter, data any) {
	Write(w, http.StatusOK, newBody(CodeSuccess, data, nil))
}

// Created mengirim response resource baru (code 0, HTTP 201).
func Created(w http.ResponseWriter, data any) {
	Write(w, http.StatusCreated, newBody(CodeSuccess, data, nil))
}

// PartialSuccess mengirim response operasi bulk dengan kegagalan sebagian
// (code 1, HTTP 200).
func PartialSuccess(w http.ResponseWriter, data any, errs []Detail) {
	Write(w, http.StatusOK, newBody(CodePartialFailure, data, errs))
}

// Error mengirim response error dengan kode aplikasi dan daftar detail.
// HTTP status diturunkan dari kode, data selalu null.
func Error(w http.ResponseWriter, code int, errs []Detail) {
	Write(w, HTTPStatus(code), newBody(code, nil, errs))
}

// ErrorMsg mengirim response error dengan satu pesan tanpa field.
func ErrorMsg(w http.ResponseWriter, code int, message string) {
	Error(w, code, []Detail{{Message: message}})
}

// ErrorField mengirim response error untuk satu field.
func ErrorField(w http.ResponseWriter, code int, field, message string) {
	Error(w, code, []Detail{{Field: field, Message: message}})
}

// ValidationErrors mengirim error validasi (code 1003, HTTP 422) dengan satu
// Detail per field, terurut berdasarkan Field.
func ValidationErrors(w http.ResponseWriter, fields map[string]string) {
	Error(w, CodeValidationError, toDetails(fields))
}
