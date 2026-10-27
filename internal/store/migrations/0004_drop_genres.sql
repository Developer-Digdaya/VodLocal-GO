-- Genre dihapus: provider Densgo tidak mengirim data genre, sehingga tabel
-- referensi dan relasinya tidak lagi dipakai.
DROP TABLE IF EXISTS title_genres;
DROP TABLE IF EXISTS genres;
