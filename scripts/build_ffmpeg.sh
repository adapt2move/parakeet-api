#!/bin/sh
set -eu

# The worker decodes local audio to mono PCM. No video, network, devices or external codecs.
./configure \
    --prefix=/usr \
    --disable-autodetect --disable-everything --disable-network --disable-devices \
    --disable-doc --disable-debug --disable-x86asm --disable-shared --enable-static --enable-small \
    --disable-ffplay --disable-ffprobe --disable-avdevice --disable-swscale \
    --enable-ffmpeg --enable-zlib \
    --enable-protocol=file,pipe \
    --enable-demuxer=aac,ac3,aiff,amr,ape,asf,au,avi,caf,flac,matroska,mov,mp3,mpegps,mpegts,ogg,wav,wv \
    --enable-decoder='aac,aac_fixed,ac3,alac,amrnb,amrwb,ape,atrac3,atrac3p,dca,flac,mp1,mp1float,mp2,mp2float,mp3,mp3float,opus,vorbis,wavpack,wmalossless,wmapro,wmav1,wmav2,pcm_*,adpcm_*' \
    --enable-parser=aac,aac_latm,ac3,dca,flac,mpegaudio,opus,vorbis \
    --enable-filter=abuffer,abuffersink,aformat,anull,aresample,atrim \
    --enable-encoder=pcm_f32le --enable-muxer=pcm_f32le

make -j "${FFMPEG_BUILD_JOBS:-4}"
make DESTDIR=/out install-progs
