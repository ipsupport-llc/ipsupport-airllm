.PHONY: build test vet tidy run compose-up compose-down web-build fmt

build:
	go build -o bin/ipsupport-airouter ./cmd/ipsupport-airouter

test:
	go test ./...

vet:
	go vet ./...

tidy:
	go mod tidy

fmt:
	gofmt -w .

run:
	go run ./cmd/ipsupport-airouter

compose-up:
	docker compose -f deploy/docker-compose.yml up --build

compose-down:
	docker compose -f deploy/docker-compose.yml down -v

web-build:
	cd web && npm ci && npm run build
