.PHONY: dev build test test-race web-install web-build web-test test-e2e docker-build clean

GO_TAGS := sqlite_fts5

dev:
	go run -tags $(GO_TAGS) ./cmd/server

build: web-build
	mkdir -p bin
	go build -tags $(GO_TAGS) -trimpath -o bin/nexusmail ./cmd/server

test:
	go test -tags $(GO_TAGS) ./...
	cd web && npm test

test-race:
	go test -tags $(GO_TAGS) -race ./...

web-install:
	cd web && npm ci

web-build:
	cd web && npm run build
	mkdir -p internal/transport/http/static/dist
	cp -R web/dist/. internal/transport/http/static/dist/

web-test:
	cd web && npm test

test-e2e:
	cd web && npm run test:e2e

docker-build:
	docker build -t nexusmail:local .

clean:
	rm -rf bin web/dist coverage
