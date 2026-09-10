Playback regression checks:

```sh
node tests/remux.test.mjs
(cd streamer && go test ./... && go vet ./...)
(cd admin && go test ./... && go vet ./...)
(cd viewer && go test ./... && go vet ./...)
```

The JavaScript tests use asynchronous MSE and packet-source doubles to exercise
buffering, cleanup, errors, source switching, and seeks without a browser or npm
installation. They also enforce identical admin/viewer remux modules. They do
not establish actual browser decoder support.

The Go integration tests generate short MKV files with MPEG-4 video and optional
AC3 audio, call the production conversion function, and decode its H.264/AAC
output with FFmpeg. Install `ffmpeg` and `ffprobe` to run these cases; other Go
regressions do not need those binaries.

Browser remux playback remains range-seekable. The server compatibility fallback
(`?transcode=1`) uses CPU and produces sequential fragmented MP4; it has no seek
protocol. Its stdin pipe cannot handle every input requiring backward reads.
