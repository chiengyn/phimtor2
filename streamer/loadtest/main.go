// Command loadtest drives the streamer with many concurrent viewers to measure
// how many it can serve. The interesting case is -scatter: many clients reading
// the *same* file at *different* offsets, which is exactly what the single
// versus multi playhead cache eviction affects. The key output is how much was
// pulled from the swarm (bytesCompleted delta) versus served from cache — a good
// multi-reader cache keeps swarm download roughly flat as viewers increase.
//
//	go run ./loadtest -url http://localhost:8080 -magnet 'magnet:?...' -n 30 -scatter
//	go run ./loadtest -infohash <hex> -n 50 -bitrate 1.5 -duration 60s -scatter
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type fileInfo struct {
	Index   int    `json:"index"`
	Path    string `json:"path"`
	Length  int64  `json:"length"`
	IsVideo bool   `json:"isVideo"`
}

type torrentInfo struct {
	InfoHash string     `json:"infoHash"`
	Name     string     `json:"name"`
	Files    []fileInfo `json:"files"`
}

type torrentStats struct {
	BytesCompleted int64 `json:"bytesCompleted"`
	TotalBytes     int64 `json:"totalBytes"`
}

func main() {
	var (
		base     = flag.String("url", "http://localhost:8080", "streamer base URL")
		magnet   = flag.String("magnet", "", "magnet link to add (otherwise use -infohash)")
		infoHash = flag.String("infohash", "", "infoHash of an already-added torrent")
		fileIdx  = flag.Int("file", -1, "file index to stream (default: largest video file)")
		n        = flag.Int("n", 20, "number of concurrent viewers")
		scatter  = flag.Bool("scatter", false, "start each viewer at a different offset (realistic many-users pattern)")
		bitrate  = flag.Float64("bitrate", 1.5, "per-viewer playback rate in MB/s (paced, simulates real playback)")
		duration = flag.Duration("duration", 60*time.Second, "how long each viewer streams")
		timeout  = flag.Duration("ready-timeout", 60*time.Second, "how long to wait for torrent metadata")
	)
	flag.Parse()

	client := &http.Client{}
	api := strings.TrimRight(*base, "/") + "/api/torrents"

	ih := *infoHash
	if *magnet != "" {
		var err error
		if ih, err = addMagnet(client, api, *magnet); err != nil {
			log.Fatalf("add magnet: %v", err)
		}
		log.Printf("added torrent %s", ih)
	}
	if ih == "" {
		log.Fatal("provide -magnet or -infohash")
	}

	info, err := waitReady(client, api, ih, *timeout)
	if err != nil {
		log.Fatalf("wait for metadata: %v", err)
	}
	idx := *fileIdx
	if idx < 0 {
		idx = largestVideo(info.Files)
	}
	if idx < 0 || idx >= len(info.Files) {
		log.Fatalf("no streamable file (idx=%d, files=%d)", idx, len(info.Files))
	}
	f := info.Files[idx]
	log.Printf("streaming %q (%s, file %d) with %d viewers, scatter=%v, %.2f MB/s each for %s",
		info.Name, humanBytes(f.Length), idx, *n, *scatter, *bitrate, *duration)

	before := stats(client, api, ih)

	streamURL := fmt.Sprintf("%s/%s/files/%d/stream", api, ih, idx)
	var (
		wg          sync.WaitGroup
		totalBytes  atomic.Int64
		totalStalls atomic.Int64
		failures    atomic.Int64
	)
	start := time.Now()
	for i := 0; i < *n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var offset int64
			if *scatter && f.Length > 0 {
				offset = int64(i) * f.Length / int64(*n)
			}
			read, stalls, err := streamOne(client, streamURL, offset, *bitrate, *duration)
			totalBytes.Add(read)
			totalStalls.Add(int64(stalls))
			if err != nil {
				failures.Add(1)
				log.Printf("viewer %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	after := stats(client, api, ih)
	swarmDelta := after.BytesCompleted - before.BytesCompleted
	served := totalBytes.Load()

	fmt.Println("\n==== load test result ====")
	fmt.Printf("viewers:           %d (scatter=%v)\n", *n, *scatter)
	fmt.Printf("wall time:         %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("served to viewers: %s  (%.1f MB/s aggregate)\n", humanBytes(served), mbps(served, elapsed))
	fmt.Printf("swarm download:    %s  (delta of torrent bytesCompleted)\n", humanBytes(swarmDelta))
	if served > 0 {
		// >1.0 means data was served from cache more than it was pulled from the
		// swarm — the whole point of multi-reader retention.
		fmt.Printf("cache reuse:       %.2fx (served / swarm download)\n", float64(served)/float64(max64(swarmDelta, 1)))
	}
	fmt.Printf("stalls:            %d (read windows that fell behind playback rate)\n", totalStalls.Load())
	fmt.Printf("failed viewers:    %d\n", failures.Load())
}

// streamOne reads the stream from offset, pacing consumption to bitrate MB/s for
// the given duration, and counts stalls (one-second windows where the server
// couldn't keep up with the playback rate). Returns bytes read and stall count.
func streamOne(client *http.Client, url string, offset int64, bitrateMBps float64, dur time.Duration) (int64, int, error) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return 0, 0, fmt.Errorf("unexpected status %s", resp.Status)
	}

	bytesPerSec := int64(bitrateMBps * 1024 * 1024)
	if bytesPerSec <= 0 {
		bytesPerSec = 1
	}
	buf := make([]byte, 64*1024)
	var read int64
	var stalls int
	deadline := time.Now().Add(dur)

	// Consume in 1-second budgets; if a budget's worth of bytes takes longer than
	// a second to arrive, playback would have stalled.
	for time.Now().Before(deadline) {
		windowStart := time.Now()
		var windowRead int64
		for windowRead < bytesPerSec {
			toRead := bytesPerSec - windowRead
			if toRead > int64(len(buf)) {
				toRead = int64(len(buf))
			}
			nr, rerr := resp.Body.Read(buf[:toRead])
			windowRead += int64(nr)
			read += int64(nr)
			if rerr == io.EOF {
				return read, stalls, nil
			}
			if rerr != nil {
				return read, stalls, rerr
			}
		}
		elapsed := time.Since(windowStart)
		if elapsed > time.Second {
			stalls++ // couldn't deliver one second of video within one second
		} else {
			time.Sleep(time.Second - elapsed) // pace to playback rate
		}
	}
	return read, stalls, nil
}

func addMagnet(client *http.Client, api, magnet string) (string, error) {
	body, _ := json.Marshal(map[string]string{"magnet": magnet})
	resp, err := client.Post(api, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("status %s: %s", resp.Status, b)
	}
	var out struct {
		InfoHash string `json:"infoHash"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.InfoHash, nil
}

func waitReady(client *http.Client, api, ih string, timeout time.Duration) (*torrentInfo, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(api + "/" + ih)
		if err == nil && resp.StatusCode == http.StatusOK {
			var info torrentInfo
			derr := json.NewDecoder(resp.Body).Decode(&info)
			resp.Body.Close()
			if derr == nil && len(info.Files) > 0 {
				return &info, nil
			}
		} else if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(time.Second)
	}
	return nil, fmt.Errorf("metadata not ready within %s", timeout)
}

func stats(client *http.Client, api, ih string) torrentStats {
	resp, err := client.Get(api + "/" + ih + "/stats")
	if err != nil {
		return torrentStats{}
	}
	defer resp.Body.Close()
	var s torrentStats
	_ = json.NewDecoder(resp.Body).Decode(&s)
	return s
}

func largestVideo(files []fileInfo) int {
	best, bestLen := -1, int64(-1)
	for _, f := range files {
		if f.IsVideo && f.Length > bestLen {
			best, bestLen = f.Index, f.Length
		}
	}
	return best
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func mbps(b int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(b) / (1024 * 1024) / d.Seconds()
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
