GO_DOCKER = docker run --rm -v "$(CURDIR):/src" -w /src golang:1.24-alpine

.PHONY: test race vet tidy migrate-up migrate-down schema-test

test:
	$(GO_DOCKER) go test ./...

race:
	$(GO_DOCKER) go test -race ./...

vet:
	$(GO_DOCKER) go vet ./...

tidy:
	$(GO_DOCKER) go mod tidy

migrate-up:
	docker compose --profile tools run --rm migrate -path /migrations -database "$$DATABASE_URL" up

migrate-down:
	docker compose --profile tools run --rm migrate -path /migrations -database "$$DATABASE_URL" down 1

schema-test: migrate-up
	docker compose exec -T postgres psql -v ON_ERROR_STOP=1 -U challenge -d challenge -f - < db/tests/financial_schema.sql
