-- Menyimpan pilihan konten per run agar bisa di-retry tanpa memilih ulang.
ALTER TABLE sync_runs ADD COLUMN selection_json TEXT;
