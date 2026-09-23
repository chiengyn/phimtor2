#!/usr/bin/env bash
# Generates the three files the fake streamer serves. ~60 MB, not committed.
#
#   happy.mkv   H.264 + two AAC tracks (vie, eng) + an embedded SRT (eng), with a
#               burned-in clock so a screenshot proves the playback position.
#               Cues are left at the END of the file — the common case for torrent
#               releases — so the player must range-read near EOF for the index.
#   ac3.mkv     The same picture with AC-3 audio. AOSP ships no AC-3 decoder, so
#               on the emulator this must trigger the app's transcode fallback.
#   compat.mp4  What the streamer's ?transcode=1 would emit for ac3.mkv:
#               fragmented MP4, H.264 + stereo AAC.
set -euo pipefail
cd "$(dirname "$0")"
FONT=${FONT:-/usr/share/fonts/liberation/LiberationSans-Bold.ttf}
sed "s|FONTFILE|$FONT|" clock.filter > .clock.filter

ffmpeg -loglevel error -y \
  -f lavfi -i "testsrc2=size=1280x720:rate=24:duration=120" \
  -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=120" \
  -f lavfi -i "sine=frequency=660:sample_rate=48000:duration=120" \
  -i embedded.srt \
  -/filter_complex .clock.filter \
  -map "[v]" -map 1:a -map 2:a -map 3:s \
  -c:v libx264 -profile:v main -preset veryfast -b:v 1200k -g 48 -keyint_min 48 -sc_threshold 0 -pix_fmt yuv420p \
  -c:a aac -b:a 96k -ac 2 -c:s srt \
  -metadata:s:a:0 language=vie -metadata:s:a:0 "title=Tiếng Việt" \
  -metadata:s:a:1 language=eng -metadata:s:a:1 "title=English" \
  -metadata:s:s:0 language=eng -metadata:s:s:0 "title=English (embedded)" \
  happy.mkv
ffmpeg -loglevel error -y -i happy.mkv -map 0:v -map 0:a:0 -c:v copy -c:a ac3 -b:a 192k ac3.mkv
ffmpeg -loglevel error -y -i ac3.mkv -c:v copy -c:a aac -b:a 128k -ac 2 \
  -movflags frag_keyframe+empty_moov+default_base_moof -f mp4 compat.mp4
rm -f .clock.filter
ls -la happy.mkv ac3.mkv compat.mp4
