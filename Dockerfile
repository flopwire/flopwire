FROM --platform=$BUILDPLATFORM node:24-bookworm-slim AS web-build
RUN corepack enable
WORKDIR /src/web
COPY web/package.json web/pnpm-lock.yaml ./
RUN --mount=type=cache,target=/root/.local/share/pnpm/store pnpm install --frozen-lockfile
COPY web/ .
RUN pnpm build

FROM --platform=$BUILDPLATFORM golang:1.26.6-bookworm AS go-build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN rm -rf internal/webapp/dist && mkdir -p internal/webapp/dist
COPY --from=web-build /src/web/dist/ internal/webapp/dist/
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
