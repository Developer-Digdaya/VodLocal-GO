-- 0007_devices_note.sql
--
-- Kolom devices.name diganti menjadi note: isinya bukan nama perangkat,
-- melainkan catatan bebas opsional dari operator. SQLite 3.25+ mendukung
-- RENAME COLUMN tanpa membangun ulang tabel, sehingga playback_progress
-- (foreign key ke devices.id) tidak tersentuh.

ALTER TABLE devices RENAME COLUMN name TO note;
