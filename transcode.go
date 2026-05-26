package main

import (
	"context"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
)

var browserNativeExts = map[string]bool{
	".mp4":  true,
	".webm": true,
	".ogg":  true,
	".ogv":  true,
}

func needsTranscode(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return !browserNativeExts[ext]
}

func detectContentType(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".ogg", ".ogv":
		return "video/ogg"
	default:
		return "video/mp4"
	}
}

// transcodeStream pipes reader through FFmpeg and writes fragmented MP4 to w.
// The output is browser-compatible regardless of input container/codec.
func transcodeStream(ctx context.Context, reader io.ReadSeeker, w http.ResponseWriter) error {
	args := []string{
		"-i", "pipe:0",
		"-c:v", "copy", // try codec copy first (works if video is already H.264)
		"-c:a", "aac",
		"-f", "mp4",
		"-movflags", "frag_keyframe+empty_moov+faststart",
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
