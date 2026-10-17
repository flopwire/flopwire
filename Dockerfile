FROM --platform=$BUILDPLATFORM golang:1.27.2-bookworm AS go-build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" -o /out/flopwire ./cmd/flopwire

# trixie ships postgresql-client 17, matching the Compose Postgres major so
# pg_dump can back it up.
FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates postgresql-client \
    && rm -rf /var/lib/apt/lists/*
COPY --from=go-build /out/flopwire /usr/local/bin/flopwire
RUN groupadd --system --gid 999 flopwire \
    && useradd --system --uid 10001 --gid flopwire --create-home flopwire \
    && install -d -o flopwire -g flopwire -m 0700 /var/lib/flopwire
RUN flopwire version && pg_dump --version
USER flopwire
EXPOSE 8080
ENTRYPOINT ["flopwire"]
CMD ["serve"]
