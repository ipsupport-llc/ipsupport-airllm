.PHONY: build test test-race vet tidy run compose-up compose-down fmt helm-lint

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

# Lint + render the Helm chart across every value permutation (no cluster needed).
helm-lint:
	@for f in deploy/helm/airllm/ci/*-values.yaml; do \
		echo "== $$f =="; \
		helm lint deploy/helm/airllm -f $$f || exit 1; \
		helm template airllm deploy/helm/airllm -f $$f >/dev/null || exit 1; \
	done
	@echo "helm chart OK"
