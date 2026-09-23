// fake: a stand-in manager + streamer for end-to-end testing the TV app without
// real torrents. It honours the real streamer's public contract closely enough
// to catch the mistakes that matter:
//
//   - stats 404s until "metadata" arrives (the first two polls), like a fresh torrent
//   - ?raw=1 serves the file with real Range support (http.ServeContent)
//   - ?transcode=1 serves a pre-made fMP4 sequentially, ignoring Range, like ffmpeg
//   - a stream request with NEITHER is logged loudly and refused: the real streamer
//     would have silently transcoded it, which is the bug this test exists to catch
//   - HEAD is 405, as on the real chi router
//
// Every request is logged, so the log is the evidence.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var btih = regexp.MustCompile(`(?i)btih:([0-9a-f]{40})`)

// THROTTLE_KBPS caps each raw stream, like a swarm rather than a LAN. Without
// it the player buffers the whole file from the first request, and no seek ever
// needs a new range — which would prove nothing about range-based seeking.
var throttleKBps, _ = strconv.Atoi(os.Getenv("THROTTLE_KBPS"))

type slowWriter struct{ http.ResponseWriter }

func (s slowWriter) Write(p []byte) (int, error) {
	const chunk = 16 << 10
	n := 0
	for len(p) > 0 {
		c := min(chunk, len(p))
		m, err := s.ResponseWriter.Write(p[:c])
		n += m
		if err != nil {
			return n, err
		}
		time.Sleep(time.Duration(c) * time.Second / time.Duration(throttleKBps<<10))
		p = p[c:]
	}
	return n, nil
}

func throttled(w http.ResponseWriter) http.ResponseWriter {
	if throttleKBps <= 0 {
		return w
	}
	return slowWriter{w}
}

func main() {
	media := os.Args[1]
	files := map[string]string{"0": filepath.Join(media, "happy.mkv"), "1": filepath.Join(media, "ac3.mkv")}
	compat := filepath.Join(media, "compat.mp4")

	var mu sync.Mutex
	polls := map[string]int{}

	manager := http.NewServeMux()
	manager.HandleFunc("POST /api/torrents", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m := btih.FindStringSubmatch(string(body))
		if m == nil {
			http.Error(w, `{"error":"no magnet"}`, 400)
			return
		}
		hash := strings.ToLower(m[1])
		log.Printf("MANAGER ADD %s", hash)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(202)
		json.NewEncoder(w).Encode(map[string]string{"infoHash": hash, "streamerPublicURL": "http://10.0.2.2:18090"})
	})
	manager.HandleFunc("DELETE /api/torrents/{hash}", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("MANAGER DROP %s   <- the viewer released the torrent (leave beacon or heartbeat TTL)", r.PathValue("hash"))
		w.WriteHeader(204)
	})

	streamer := http.NewServeMux()
	streamer.HandleFunc("/api/torrents/{hash}/stats", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(405)
			return
		}
		hash := r.PathValue("hash")
		mu.Lock()
		polls[hash]++
		n := polls[hash]
		mu.Unlock()
		if n <= 2 {
			log.Printf("STREAMER STATS %s poll=%d -> 404 (no metadata yet)", hash, n)
			http.Error(w, `{"error":"torrent not found"}`, 404)
			return
		}
		st, _ := os.Stat(files["0"])
		log.Printf("STREAMER STATS %s poll=%d -> ready", hash, n)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"infoHash": hash, "totalBytes": st.Size(), "bytesCompleted": 0,
			"activePeers": 7, "totalPeers": 12, "connectedSeeders": 3,
		})
	})
	streamer.HandleFunc("/api/torrents/{hash}/files/{idx}/stream", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			log.Printf("STREAMER %s -> 405 (the real streamer only routes GET)", r.Method)
			w.WriteHeader(405)
			return
		}
		idx := r.PathValue("idx")
		q := r.URL.Query()
		rng := r.Header.Get("Range")
		switch {
		case q.Get("raw") == "1":
			path, ok := files[idx]
			if !ok {
				http.NotFound(w, r)
				return
			}
			log.Printf("STREAMER STREAM raw  file=%s range=%q", filepath.Base(path), rng)
			f, err := os.Open(path)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			defer f.Close()
			w.Header().Set("Content-Type", "video/x-matroska")
			http.ServeContent(throttled(w), r, path, time.Time{}, f)
		case q.Get("transcode") == "1":
			log.Printf("STREAMER STREAM TRANSCODE file=%s range=%q (ignored: the transcode cannot seek)", idx, rng)
			f, err := os.Open(compat)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			defer f.Close()
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Accept-Ranges", "none")
			w.Header().Set("Cache-Control", "no-store")
			io.Copy(w, f)
		default:
			log.Printf("!!!!! STREAMER STREAM WITHOUT raw=1 — the real streamer would have silently TRANSCODED this (query=%q)", r.URL.RawQuery)
			http.Error(w, "missing raw=1", 400)
		}
	})

	cors := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			h.ServeHTTP(w, r)
		})
	}

	go func() { log.Fatal(http.ListenAndServe("0.0.0.0:18083", manager)) }()
	fmt.Println("fake manager :18083, fake streamer :18090")
	log.Fatal(http.ListenAndServe("0.0.0.0:18090", cors(streamer)))
}
