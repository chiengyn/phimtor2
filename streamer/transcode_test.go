package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTranscodeCompatibility(t *testing.T) {
	for _, binary := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("%s required for media integration test", binary)
		}
	}
	for _, audio := range []bool{false, true} {
		t.Run(map[bool]string{false: "video only", true: "AC3 audio"}[audio], func(t *testing.T) {
			input := filepath.Join(t.TempDir(), "input.mkv")
			args := []string{"-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=12:duration=1"}
			if audio {
				args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "ac3")
			}
			args = append(args, "-c:v", "mpeg4", input)
			if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
				t.Fatalf("create fixture: %v: %s", err, out)
			}
			r, err := os.Open(input)
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			if err := transcodeStream(context.Background(), r, w); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "video/mp4" || !w.Flushed {
				t.Fatalf("unexpected response: %d %v, flushed=%v", w.Code, w.Header(), w.Flushed)
			}
			if _, err := r.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("input not closed: %v", err)
			}
			probe := exec.Command("ffprobe", "-v", "error", "-count_frames", "-show_streams", "-of", "json", "pipe:0")
			probe.Stdin = bytes.NewReader(w.Body.Bytes())
			data, err := probe.Output()
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Streams []struct {
					CodecName string `json:"codec_name"`
					PixFmt    string `json:"pix_fmt"`
					Frames    string `json:"nb_read_frames"`
				}
			}
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Streams) == 0 || got.Streams[0].CodecName != "h264" || got.Streams[0].PixFmt != "yuv420p" || got.Streams[0].Frames != "12" {
				t.Fatalf("video codec or frames lost: %s", data)
			}
			if audio && (len(got.Streams) != 2 || got.Streams[1].CodecName != "aac") {
				t.Fatalf("missing AAC audio: %s", data)
			}
			decode := exec.Command("ffmpeg", "-v", "error", "-xerror", "-i", "pipe:0", "-f", "null", "-")
			decode.Stdin = bytes.NewReader(w.Body.Bytes())
			if out, err := decode.CombinedOutput(); err != nil {
				t.Fatalf("decode output: %v: %s", err, out)
			}
		})
	}
}

// A fake process makes disconnect/early-exit tests deterministic without a swarm.
func fakeFFmpeg(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

type blockedInput struct {
	closed chan struct{}
	once   sync.Once
}

func (r *blockedInput) Read([]byte) (int, error) { <-r.closed; return 0, io.EOF }
func (r *blockedInput) Close() error             { r.once.Do(func() { close(r.closed) }); return nil }

func TestTranscodeEarlyExitClosesBlockedInput(t *testing.T) {
	fakeFFmpeg(t, "echo 'unsupported input' >&2\nexit 1\n")
	r := &blockedInput{closed: make(chan struct{})}
	w := httptest.NewRecorder()
	done := make(chan error, 1)
	go func() { done <- transcodeStream(context.Background(), r, w) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "unsupported input") {
			t.Fatalf("missing diagnostic: %v", err)
		}
		if w.Code != http.StatusBadGateway {
			t.Fatalf("status %d", w.Code)
		}
	case <-time.After(3 * time.Second):
		r.Close()
		t.Fatal("FFmpeg exit left stdin copy blocked")
	}
}

func TestTranscodeCancellation(t *testing.T) {
	fakeFFmpeg(t, "while :; do :; done\n")
	ctx, cancel := context.WithCancel(context.Background())
	r := &blockedInput{closed: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- transcodeStream(ctx, r, httptest.NewRecorder()) }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation")
		}
	case <-time.After(3 * time.Second):
		r.Close()
		t.Fatal("cancelled transcode did not stop")
	}
}

type failedWriter struct{ *httptest.ResponseRecorder }

func (failedWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestTranscodeWriteFailure(t *testing.T) {
	fakeFFmpeg(t, "printf 'fragment'\nwhile :; do :; done\n")
	r := &blockedInput{closed: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- transcodeStream(context.Background(), r, failedWriter{httptest.NewRecorder()}) }()
	select {
	case err := <-done:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("write error lost: %v", err)
		}
	case <-time.After(3 * time.Second):
		r.Close()
		t.Fatal("write failure did not terminate FFmpeg")
	}
}

func TestTranscodeMissingExecutable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	w := httptest.NewRecorder()
	if err := transcodeStream(context.Background(), io.NopCloser(strings.NewReader("")), w); err == nil {
		t.Fatal("expected missing executable error")
	}
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status %d", w.Code)
	}
}
