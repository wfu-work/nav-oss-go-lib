WEB_ZIP ?= ../nav-oss-lib-web/dist/nav-oss-lib-web.zip
WEB_API_ADDR ?= 127.0.0.1:9000
WEB_CONSOLE_ADDR ?= 127.0.0.1:9001
WEB_CONFIG ?=

.PHONY: test test-race vet verify coverage web-sync web-run web-verify

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

verify: test test-race vet

coverage:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

web-sync:
	./scripts/sync-webs.sh "$(WEB_ZIP)"

web-run:
	go run ./cmd/nav-oss-web -api-addr "$(WEB_API_ADDR)" -console-addr "$(WEB_CONSOLE_ADDR)" $(if $(WEB_CONFIG),-config "$(WEB_CONFIG)")

web-verify:
	go build ./...
