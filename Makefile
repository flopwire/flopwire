.PHONY: test build web e2e e2e-sync

test:
	go test -race ./...
	pnpm --dir web test
	scripts/validate-release-tag_test.sh
	scripts/perf-baseline_test.sh

web:
	pnpm --dir web build
	rm -rf internal/webapp/dist/assets
	cp web/dist/index.html internal/webapp/dist/index.html
	cp -R web/dist/assets internal/webapp/dist/assets

VERSION ?= dev

build: web
	go build -trimpath -ldflags="-X main.version=$(VERSION)" -o bin/flopwire ./cmd/flopwire

e2e:
	./scripts/e2e.sh

e2e-sync:
	./scripts/e2e-sync.sh
