package response

// Registry kode aplikasi. Sumber kebenaran: docs/standar-response.md §6.
//
// Rentang kode dibagi per domain agar tidak bertabrakan:
//
//	0–1        Sukses
//	1000–1999  Umum / klien
//	2000–2999  Auth & device
//	3000–3999  Katalog & playback
//	4000–4999  Sync
//	5000–5999  Storage & generation
//	6000–6999  Upstream / provider
//	7000–7999  Media
//	8000–8999  Progress
//	9000+      Dicadangkan
const (
	// Sukses (§6.1).
	CodeSuccess        = 0
	CodePartialFailure = 1

	// Umum (§6.2).
	CodeInternalError        = 1000
	CodeBadRequest           = 1001
	CodeNotFound             = 1002
	CodeValidationError      = 1003
	CodeMethodNotAllowed     = 1004
	CodePayloadTooLarge      = 1005
	CodeUnsupportedMediaType = 1006
	CodeRateLimited          = 1007
	CodeServiceUnavailable   = 1008
	CodeRequestTimeout       = 1009
	CodeDatabaseError        = 1010

	// Auth & device (§6.3).
	CodeUnauthorized            = 2000
	CodeForbidden               = 2001
	CodeDeviceNotFound          = 2002
	CodeDeviceAlreadyRegistered = 2003
	CodeDeviceDisabled          = 2004
	CodeSessionExpired          = 2005
	CodeInvalidCredentials      = 2006
	CodeCSRFInvalid             = 2007
	CodeTokenInvalid            = 2008

	// Katalog & playback (§6.4).
	CodeTitleNotFound       = 3000
	CodeEpisodeNotFound     = 3001
	CodePlayableNotFound    = 3002
	CodePlayableNotReady    = 3003
	CodeGenreNotFound       = 3004
	CodeSearchQueryInvalid  = 3005
	CodeContentNotPublished = 3006

	// Sync (§6.5).
	CodeSyncNotFound           = 4000
	CodeSyncAlreadyRunning     = 4001
	CodeSyncNotCancellable     = 4002
	CodeProviderError          = 4003
	CodeProviderDisabled       = 4004
	CodePlaylistParseError     = 4005
	CodeSegmentDownloadFailed  = 4006
	CodeVerificationFailed     = 4007
	CodeGenerationSwitchFailed = 4008
	CodeNoActiveGeneration     = 4009

	// Storage & generation (§6.6).
	CodeInsufficientStorage     = 5000
	CodeGenerationActive        = 5001
	CodeGenerationNotFound      = 5002
	CodeGenerationNotDeletable  = 5003
	CodeGenerationMissingOnDisk = 5004

	// Upstream (§6.7).
	CodeUpstreamUnavailable = 6000
	CodeUpstreamTimeout     = 6001
	CodeUpstreamAuthFailed  = 6002
	CodeUpstreamRateLimited = 6003
	CodeUpstreamBadResponse = 6004

	// Media (§6.8).
	CodeMediaNotFound       = 7000
	CodeInvalidMediaPath    = 7001
	CodeRangeNotSatisfiable = 7002

	// Progress (§6.9).
	CodeProgressNotFound = 8000
	CodeProgressInvalid  = 8001
)

// defaultLabel dipakai untuk kode yang tidak terdaftar di labels.
const defaultLabel = "Terjadi kesalahan pada server"

// labels memetakan kode aplikasi ke pesan baku (msg). Setiap kode di §6 wajib
// punya entri di sini agar msg tidak pernah ditulis manual di handler.
var labels = map[int]string{
	CodeSuccess:        "Berhasil",
	CodePartialFailure: "Berhasil sebagian",

	CodeInternalError:        "Terjadi kesalahan pada server",
	CodeBadRequest:           "Permintaan tidak valid",
	CodeNotFound:             "Data tidak ditemukan",
	CodeValidationError:      "Data yang dikirim tidak valid",
	CodeMethodNotAllowed:     "Metode tidak diizinkan",
	CodePayloadTooLarge:      "Ukuran permintaan terlalu besar",
	CodeUnsupportedMediaType: "Tipe konten tidak didukung",
	CodeRateLimited:          "Terlalu banyak permintaan",
	CodeServiceUnavailable:   "Layanan tidak tersedia",
	CodeRequestTimeout:       "Permintaan melebihi batas waktu",
	CodeDatabaseError:        "Kesalahan basis data",

	CodeUnauthorized:            "Autentikasi diperlukan",
	CodeForbidden:               "Tidak memiliki akses",
	CodeDeviceNotFound:          "Perangkat tidak ditemukan",
	CodeDeviceAlreadyRegistered: "Perangkat sudah terdaftar",
	CodeDeviceDisabled:          "Perangkat dinonaktifkan",
	CodeSessionExpired:          "Sesi telah berakhir",
	CodeInvalidCredentials:      "Nama pengguna atau kata sandi salah",
	CodeCSRFInvalid:             "Token keamanan tidak valid",
	CodeTokenInvalid:            "Token tidak valid",

	CodeTitleNotFound:       "Judul tidak ditemukan",
	CodeEpisodeNotFound:     "Episode tidak ditemukan",
	CodePlayableNotFound:    "Media tidak ditemukan",
	CodePlayableNotReady:    "Media belum selesai diunduh",
	CodeGenreNotFound:       "Genre tidak ditemukan",
	CodeSearchQueryInvalid:  "Kata kunci pencarian tidak valid",
	CodeContentNotPublished: "Konten tidak tersedia",

	CodeSyncNotFound:           "Proses sinkronisasi tidak ditemukan",
	CodeSyncAlreadyRunning:     "Masih ada sinkronisasi yang berjalan",
	CodeSyncNotCancellable:     "Sinkronisasi tidak dapat dibatalkan",
	CodeProviderError:          "Penyedia konten bermasalah",
	CodeProviderDisabled:       "Penyedia konten dinonaktifkan",
	CodePlaylistParseError:     "Playlist tidak dapat dibaca",
	CodeSegmentDownloadFailed:  "Gagal mengunduh segmen media",
	CodeVerificationFailed:     "Verifikasi media gagal",
	CodeGenerationSwitchFailed: "Gagal mengaktifkan versi media baru",
	CodeNoActiveGeneration:     "Belum ada versi media aktif",

	CodeInsufficientStorage:     "Ruang penyimpanan tidak mencukupi",
	CodeGenerationActive:        "Versi media aktif tidak dapat dihapus",
	CodeGenerationNotFound:      "Versi media tidak ditemukan",
	CodeGenerationNotDeletable:  "Versi media tidak dapat dihapus",
	CodeGenerationMissingOnDisk: "Berkas versi media tidak ditemukan di disk",

	CodeUpstreamUnavailable: "Layanan penyedia tidak tersedia",
	CodeUpstreamTimeout:     "Penyedia tidak merespons tepat waktu",
	CodeUpstreamAuthFailed:  "Autentikasi ke penyedia gagal",
	CodeUpstreamRateLimited: "Penyedia membatasi permintaan",
	CodeUpstreamBadResponse: "Respons penyedia tidak valid",

	CodeMediaNotFound:       "Berkas media tidak ditemukan",
	CodeInvalidMediaPath:    "Path media tidak valid",
	CodeRangeNotSatisfiable: "Rentang byte tidak dapat dipenuhi",

	CodeProgressNotFound: "Riwayat tonton tidak ditemukan",
	CodeProgressInvalid:  "Data posisi tonton tidak valid",
}

// httpStatus memetakan kode aplikasi ke HTTP status. Sumber: §5.
var httpStatus = map[int]int{
	CodeSuccess:        200,
	CodePartialFailure: 200,

	CodeInternalError:        500,
	CodeBadRequest:           400,
	CodeNotFound:             404,
	CodeValidationError:      422,
	CodeMethodNotAllowed:     405,
	CodePayloadTooLarge:      413,
	CodeUnsupportedMediaType: 415,
	CodeRateLimited:          429,
	CodeServiceUnavailable:   503,
	CodeRequestTimeout:       408,
	CodeDatabaseError:        500,

	CodeUnauthorized:            401,
	CodeForbidden:               403,
	CodeDeviceNotFound:          404,
	CodeDeviceAlreadyRegistered: 409,
	CodeDeviceDisabled:          403,
	CodeSessionExpired:          401,
	CodeInvalidCredentials:      401,
	CodeCSRFInvalid:             403,
	CodeTokenInvalid:            401,

	CodeTitleNotFound:       404,
	CodeEpisodeNotFound:     404,
	CodePlayableNotFound:    404,
	CodePlayableNotReady:    409,
	CodeGenreNotFound:       404,
	CodeSearchQueryInvalid:  422,
	CodeContentNotPublished: 404,

	CodeSyncNotFound:           404,
	CodeSyncAlreadyRunning:     409,
	CodeSyncNotCancellable:     409,
	CodeProviderError:          502,
	CodeProviderDisabled:       409,
	CodePlaylistParseError:     422,
	CodeSegmentDownloadFailed:  502,
	CodeVerificationFailed:     422,
	CodeGenerationSwitchFailed: 500,
	CodeNoActiveGeneration:     503,

	CodeInsufficientStorage:     409,
	CodeGenerationActive:        409,
	CodeGenerationNotFound:      404,
	CodeGenerationNotDeletable:  409,
	CodeGenerationMissingOnDisk: 500,

	CodeUpstreamUnavailable: 503,
	CodeUpstreamTimeout:     504,
	CodeUpstreamAuthFailed:  502,
	CodeUpstreamRateLimited: 429,
	CodeUpstreamBadResponse: 502,

	CodeMediaNotFound:       404,
	CodeInvalidMediaPath:    400,
	CodeRangeNotSatisfiable: 416,

	CodeProgressNotFound: 404,
	CodeProgressInvalid:  422,
}
