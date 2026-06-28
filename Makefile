.PHONY: build test test-race vet tidy run compose-up compose-down fmt

build:
	go build -o bin/ipsupport-airllm ./cmd/ipsupport-airllm

test:
	go test ./...

test-race:
	go test -race ./...

vet:
	go vet ./...

tidy:
	go mod tidy

fmt:
	gofmt -w .

run:
	go run ./cmd/ipsupport-airllm

compose-up:
	docker compose -f deploy/docker-compose.yml up --build

compose-down:
	docker compose -f deploy/docker-compose.yml down -v
