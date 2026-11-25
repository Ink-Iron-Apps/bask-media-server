# Bask Media Server in a container.
#
# The build is static because the SQLite driver is pure Go, so the runtime
# image needs no libc and carries ffmpeg only — which is there for converting
# files a client cannot play and for finding intro and credits markers. Direct
# play needs neither, so a smaller image without it would still work for most
# of what this does; it is included because being told to install something
# after the fact is exactly what this project is trying to avoid.

# On the runner's own architecture, whatever is being built for: compiling Go
# for another one is a cross-compile, and emulating that architecture to run a
# native compiler in it costs tens of minutes for nothing.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src

# Dependencies first, so editing the source does not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=0.0.0-container
ARG TARGETARCH
RUN CGO_ENABLED=0 GOARCH=${TARGETARCH:-amd64} go build -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/bms ./cmd/bms

FROM alpine:3.22
RUN apk add --no-cache ffmpeg ca-certificates tzdata

COPY --from=build /out/bms /usr/local/bin/bms
COPY packaging/bms.conf.example /usr/share/bask-media-server/bms.conf.example

# The database, artwork cache and backups live here. Mount a volume over it or
# everything is lost with the container.
VOLUME /var/lib/bask-media-server
EXPOSE 32500

# Runs as a non-root user, so the media mounted in has to be readable by it —
# pass --user to match the owner of those files when that is easier than
# changing their permissions.
RUN addgroup -S bms && adduser -S -G bms -H -D bms \
 && mkdir -p /var/lib/bask-media-server \
 && chown bms:bms /var/lib/bask-media-server
USER bms

# 0.0.0.0 rather than the usual loopback default: a container that listens only
# on its own loopback cannot be reached at all.
ENTRYPOINT ["/usr/local/bin/bms"]
CMD ["-db", "/var/lib/bask-media-server/bms.db", "-addr", "0.0.0.0:32500", "-scan"]
