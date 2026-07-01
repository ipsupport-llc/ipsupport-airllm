.PHONY: build test test-race vet tidy run compose-up compose-down fmt helm-lint gen-secrets compose-prod-up compose-prod-down check-links

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

# Generate deploy/.env with fresh secrets for the production stack (compose.prod.yaml).
gen-secrets:
	@test ! -f deploy/.env || { echo "deploy/.env already exists — refusing to overwrite (remove it first)"; exit 1; }
	@command -v openssl >/dev/null || { echo "openssl is required"; exit 1; }
	@MK=$$(openssl rand -base64 32); \
	AP=$$(openssl rand -base64 24); \
	PP=$$(openssl rand -hex 24); \
	sed -e "s|^AIRLLM_MASTER_KEY=.*|AIRLLM_MASTER_KEY=$$MK|" \
	    -e "s|^AIRLLM_ADMIN_PASSWORD=.*|AIRLLM_ADMIN_PASSWORD=$$AP|" \
	    -e "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=$$PP|" \
	    deploy/.env.example > deploy/.env; \
	echo "Wrote deploy/.env with generated secrets."; \
	echo "  admin username: $$(grep '^AIRLLM_ADMIN_USERNAME=' deploy/.env | cut -d= -f2)"; \
	echo "  admin password: $$AP"; \
	echo "Set DOMAIN/ACME_EMAIL in deploy/.env before using --profile tls."

# Check internal Markdown links + heading anchors across README + docs.
check-links:
	python3 scripts/check-links.py

# Production standalone stack (run `make gen-secrets` first).
compose-prod-up:
	@test -f deploy/.env || { echo "deploy/.env missing — run 'make gen-secrets' first"; exit 1; }
	docker compose -f deploy/compose.prod.yaml up -d --build

compose-prod-down:
	docker compose -f deploy/compose.prod.yaml down
