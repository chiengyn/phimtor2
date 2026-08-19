package main

import (
	"context"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
)

// browserNativeExts are the containers a browser's <video> can demux on its own,
// so they stream straight through ServeContent with working range/seek. Anything
// else either takes the client-side remux path (see handleStream's raw mode) or
// falls back to the ffmpeg transcode below.
var browserNativeExts = map[string]bool{
	".mp4":  true,
	".m4v":  true,
	".webm": true,
	".ogg":  true,
	".ogv":  true,
}

func needsTranscode(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return !browserNativeExts[ext]
}

// detectContentType names the real container. The browser-native ones let the
// <video> element pick its demuxer; the rest matter because the watch page reads
// this header off the raw-mode response to decide whether it must remux the file
// itself (anything that isn't mp4/webm/ogg) — so reporting the true container
// here, not a blanket video/mp4, is load-bearing.
func detectContentType(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".ogg", ".ogv":
		return "video/ogg"
	case ".mkv":
		return "video/x-matroska"
	case ".avi":
		return "video/x-msvideo"
	case ".mov":
		return "video/quicktime"
	case ".ts", ".m2ts":
		return "video/mp2t"
	case ".wmv":
		return "video/x-ms-wmv"
	case ".flv":
		return "video/x-flv"
	default:
		return "application/octet-stream"
	}
}

// transcodeStream pipes reader through FFmpeg and writes fragmented MP4 to w.
// The output is browser-compatible regardless of input container/codec.
//
// The flags are tuned for low time-to-first-frame on a cold torrent:
//   - -probesize/-analyzeduration cap how much input FFmpeg reads (trickling in
//     from the swarm) before it emits anything; the defaults (~5 MB / 5 s) add
//     seconds of startup stall for no benefit on a codec copy.
//   - frag_keyframe+empty_moov makes the output progressively streamable. We do
//     NOT use +faststart here: it relocates the moov atom in a final pass that
//     needs a seekable output, but our output is a non-seekable pipe, so it only
//     risks buffering/delay — the fragmented output already needs no faststart.
//   - -flush_packets 1 pushes each fragment to the browser as soon as it's ready
//     instead of letting FFmpeg buffer.
func transcodeStream(ctx context.Context, reader io.ReadSeeker, w http.ResponseWriter) error {
	args := []string{
		"-fflags", "+nobuffer",
		"-probesize", "2M",
		"-analyzeduration", "2M",
		"-i", "pipe:0",
		"-c:v", "copy", // try codec copy first (works if video is already H.264)
		"-c:a", "aac",
		"-f", "mp4",
		"-movflags", "frag_keyframe+empty_moov",
		"-flush_packets", "1",
		"pipe:1",
	}

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stdin = reader

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	go io.Copy(io.Discard, stderr)

	if err := cmd.Start(); err != nil {
		return err
	}

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)

	io.Copy(w, stdout)
	return cmd.Wait()
}
