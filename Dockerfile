FROM alpine:latest
MAINTAINER Roman Diouskine <rdiouskine@haivision.com>

ENV FFMPEG_VERSION=4.1
ENV SRT_VERSION=v1.3.2-rc.0

ENV ROOT /var/haivision/
WORKDIR $ROOT

COPY service.go src/haivision.com/service/
COPY get_status /usr/bin/

RUN \
  mkdir build && \
  echo "Installing dependencies" && \
  apk add --update build-base curl nasm yasm tar bzip2 git tcl cmake coreutils \
  zlib-dev openssl-dev lame-dev libogg-dev x264-dev libvpx-dev libvorbis-dev x265-dev freetype-dev libass-dev libwebp-dev libtheora-dev opus-dev && \
  apk add ca-certificates && update-ca-certificates && \
  echo "Installing libsrt" && \
  cd $ROOT\build && \
  git clone https://github.com/Haivision/srt.git && cd srt && \
  git checkout ${SRT_VERSION} -b ${SRT_VERSION} && \
  cmake -DCMAKE_INSTALL_PREFIX=/usr -DCMAKE_INSTALL_LIBDIR=lib -DCMAKE_INSTALL_BINDIR=bin -DCMAKE_INSTALL_INCLUDEDIR=include && \
  make install && \
  echo "Installing ffmpeg" && \
  cd $ROOT\build && \
  curl -s http://ffmpeg.org/releases/ffmpeg-${FFMPEG_VERSION}.tar.gz | tar zxf - -C . && cd ffmpeg-${FFMPEG_VERSION} && \
  ./configure --enable-gpl --enable-nonfree --enable-small --disable-debug --enable-openssl --enable-libsrt --enable-libx264 && \
  make && make install && \
  echo "Cleaning up" && \
  cd $ROOT && \
  rm -rf ./build && \
  apk del zlib-dev openssl-dev lame-dev libogg-dev x264-dev libvpx-dev libvorbis-dev x265-dev freetype-dev libass-dev libwebp-dev libtheora-dev opus-dev && \
  apk add zlib openssl lame libogg x264-libs libvpx libvorbis x265 freetype libass libwebp libtheora opus && \
  apk del build-base curl nasm yasm tar bzip2 git tcl cmake coreutils && rm -rf /var/cache/apk/*

RUN \
  mkdir build && mv src/ build/ && \
  echo "Installing Go dependencies" && \
  apk add --update build-base go git && \
  echo "Installing Go service" && \
  cd $ROOT/build && \
  export GOPATH=$PWD && go get haivision.com/service && go install haivision.com/service && \
  mv ./bin/service $ROOT && \
  echo "Cleaning up" && \
  cd $ROOT && \
  rm -rf ./build && \
  apk del build-base go git && rm -rf /var/cache/apk/*

CMD ["./service"]
