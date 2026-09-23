// seed: adds (or with "cleanup", removes) the rows the TV end-to-end test needs
// in the LOCAL dev database. Everything it writes is tagged "e2e" so cleanup can
// find it again.
//
//	seed <dsn> <subtitle-dir> seed    -> prints the movie id used
//	seed <dsn> <subtitle-dir> cleanup
package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"

	_ "github.com/go-sql-driver/mysql"
)

const (
	hash   = "e2e0000000000000000000000000000000000001"
	magnet = "magnet:?xt=urn:btih:" + hash + "&dn=e2e"
)

func main() {
	dsn, subDir, mode := os.Args[1], os.Args[2], os.Args[3]
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	if mode == "user" {
		// The throwaway account the viewer's integration tests also use.
		var id int64
		err := db.QueryRow(`SELECT id FROM users WHERE provider = 'google' AND provider_uid = 'go-test-provider-uid'`).Scan(&id)
		if err == sql.ErrNoRows {
			res, err := db.Exec(`INSERT INTO users (provider, provider_uid, email, name) VALUES ('google', 'go-test-provider-uid', 'go-test@example.invalid', 'Go Test')`)
			if err != nil {
				log.Fatal(err)
			}
			id, _ = res.LastInsertId()
		} else if err != nil {
			log.Fatal(err)
		}
		fmt.Println(id)
		return
	}

	if mode == "comp" || mode == "uncomp" {
		// Stand in for the admin's /users page granting (or revoking) the test
		// account a comp — the admin-owned users.comp_* columns.
		q := `UPDATE users SET comp_expires_at = DATE_ADD(NOW(), INTERVAL 1 DAY), comp_granted_at = NOW() WHERE provider_uid = 'go-test-provider-uid'`
		if mode == "uncomp" {
			q = `UPDATE users SET comp_expires_at = NULL, comp_granted_at = NULL WHERE provider_uid = 'go-test-provider-uid'`
		}
		if _, err := db.Exec(q); err != nil {
			log.Fatal(err)
		}
		fmt.Println(mode, "ok")
		return
	}

	if mode == "cleanup" {
		for _, q := range []string{
			`DELETE FROM subtitles WHERE provider = 'e2e'`,
			`DELETE FROM videos WHERE name LIKE 'e2e-%'`,
			`DELETE FROM torrent_sources WHERE info_hash = '` + hash + `'`,
			`DELETE FROM tv_devices WHERE user_id = (SELECT id FROM users WHERE provider_uid = 'go-test-provider-uid')`,
		} {
			res, err := db.Exec(q)
			if err != nil {
				log.Fatalf("%s: %v", q, err)
			}
			n, _ := res.RowsAffected()
			fmt.Printf("%-60s %d rows\n", q[:min(60, len(q))], n)
		}
		os.RemoveAll(filepath.Join(subDir, "e2e"))
		return
	}

	var titleID int64
	var titleName string
	if err := db.QueryRow(`SELECT id, title FROM titles WHERE type = 'movie' ORDER BY id LIMIT 1`).Scan(&titleID, &titleName); err != nil {
		log.Fatalf("pick a movie: %v", err)
	}
	res, err := db.Exec(`INSERT INTO torrent_sources (info_hash, magnet) VALUES (?, ?)
		ON DUPLICATE KEY UPDATE id = LAST_INSERT_ID(id)`, hash, magnet)
	if err != nil {
		log.Fatal(err)
	}
	sourceID, _ := res.LastInsertId()

	// 720p and 1080p are the happy file (index 0); 2160p is the AC-3 file (index 1),
	// which the emulator cannot decode — the compatibility-fallback case.
	for _, v := range []struct {
		res  string
		file int
	}{{"720p", 0}, {"1080p", 0}, {"2160p", 1}} {
		if _, err := db.Exec(`INSERT INTO videos (source_id, title_id, name, resolution, file_index, file_path, file_size)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, sourceID, titleID, "e2e-"+v.res, v.res, v.file, fmt.Sprintf("e2e/%s.mkv", v.res), 20_000_000); err != nil {
			log.Fatal(err)
		}
	}

	// A saved SRT subtitle in Vietnamese: exercises the `format` field (SubRip, not
	// WebVTT) and UTF-8 diacritics end to end.
	key := "e2e/vi.srt"
	if err := os.MkdirAll(filepath.Join(subDir, "e2e"), 0o755); err != nil {
		log.Fatal(err)
	}
	srt := "1\n00:00:01,000 --> 00:02:00,000\n[PHỤ ĐỀ ĐÃ LƯU] Tiếng Việt có dấu — xin chào!\n"
	if err := os.WriteFile(filepath.Join(subDir, key), []byte(srt), 0o644); err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO subtitles (title_id, provider, provider_file_id, language, name, format, storage_backend, storage_key)
		VALUES (?, 'e2e', 'e2e-vi', 'vi', 'Phụ đề đã lưu', 'srt', 'local', ?)`, titleID, key); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("seeded movie %d (%s)\n", titleID, titleName)
}
