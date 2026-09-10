package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

// transcodeArgs converts the selected video and optional audio to H.264/AAC.
// This is the compatibility fallback, so copying the video would preserve the
// very codec the browser rejected. Keep the original bytes on the raw path.
func transcodeArgs() []string {
	return []string{
		"-hide_banner", "-loglevel", "error", "-nostdin",
		"-probesize", "2M", "-analyzeduration", "2M",
		"-i", "pipe:0",
		"-map", "0:V:0", "-map", "0:a:0?", "-sn", "-dn",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23",
		"-threads", "2", "-tune", "zerolatency", "-pix_fmt", "yuv420p",
		"-vf", "pad=ceil(iw/2)*2:ceil(ih/2)*2", "-g", "48",
		"-c:a", "aac", "-ac", "2", "-b:a", "192k",
		"-f", "mp4", "-movflags", "frag_keyframe+empty_moov+default_base_moof",
		"-flush_packets", "1", "pipe:1",
	}
}

// transcodeStream owns and closes reader, including on disconnect and early
// FFmpeg exit. Closing the torrent reader unblocks os/exec's stdin copy; killing
// FFmpeg alone leaves Wait stuck if that copy is waiting for missing pieces.
// Output is sequential fMP4, not byte-range seekable. The pipe input also cannot
// seek: containers requiring backward reads (e.g. tail-moov MOV) need a future
// seekable input transport. Do not promise support for every possible file.
func transcodeStream(ctx context.Context, reader io.ReadCloser, w http.ResponseWriter) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var closeOnce sync.Once
	closeReader := func() { closeOnce.Do(func() { _ = reader.Close() }) }
	stopClose := context.AfterFunc(ctx, closeReader)
	defer stopClose()
	defer closeReader()

	started := false
	defer func() {
		if err != nil && !started && ctx.Err() == nil {
			http.Error(w, "video conversion failed", http.StatusBadGateway)
		}
	}()

	cmd := exec.CommandContext(ctx, "ffmpeg", transcodeArgs()...)
	cmd.Stdin = reader
	var stderr transcodeErrorBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}

	// Wait for real output before committing 200, so startup failures are visible.
	buf := make([]byte, 32*1024)
	n, readErr := stdout.Read(buf)
	var copyErr error
	if n > 0 {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Accept-Ranges", "none")
		started = true
		fw := transcodeWriter{w}
		_, copyErr = fw.Write(buf[:n])
		if copyErr == nil && readErr == nil {
			_, copyErr = io.CopyBuffer(fw, stdout, buf)
		}
	}
	if copyErr != nil || (readErr != nil && readErr != io.EOF) {
		cancel()
	}
	closeReader()
	waitErr := cmd.Wait()
	if copyErr != nil {
		return fmt.Errorf("write converted video: %w", copyErr)
	}
	if waitErr != nil {
		return fmt.Errorf("ffmpeg: %w: %s", waitErr, strings.TrimSpace(string(stderr)))
	}
	if readErr != nil && readErr != io.EOF {
		return readErr
	}
	if !started {
		return fmt.Errorf("ffmpeg produced no video: %s", strings.TrimSpace(string(stderr)))
	}
	return nil
}

// Bound diagnostics even for long or corrupt streams. Read only after cmd.Wait.
type transcodeErrorBuffer []byte

func (b *transcodeErrorBuffer) Write(p []byte) (int, error) {
	const limit = 8 * 1024
	n := len(p)
	if n >= limit {
		*b = append((*b)[:0], p[n-limit:]...)
	} else {
		if len(*b)+n > limit {
			*b = (*b)[len(*b)+n-limit:]
		}
		*b = append(*b, p...)
	}
	return n, nil
}

type transcodeWriter struct{ http.ResponseWriter }

func (w transcodeWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if err == nil {
		// ResponseController follows Unwrap through the egress meter.
		_ = http.NewResponseController(w.ResponseWriter).Flush()
	}
	return n, err
}
