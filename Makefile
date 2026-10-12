GO_DOCKER = docker run --rm -v "$(CURDIR):/src" -w /src golang:1.24

.PHONY: test race vet tidy migrate-up migrate-down schema-test integration-test verify

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

integration-test:
	docker compose up -d postgres localstack keycloak
	docker compose --profile tools run --rm migrate -path /migrations -database "postgres://challenge:challenge@postgres:5432/challenge?sslmode=disable" up
	docker run --rm --network backend-challenge-go_default --env-file .env.example -v "$(CURDIR):/src" -w /src golang:1.24 go test ./internal/core -count=1
	docker run --rm --network backend-challenge-go_default --env-file .env.example -v "$(CURDIR):/src" -w /src golang:1.24 go test ./internal/app -run 'Test(OutboxPublisher|SQSConsumer)' -count=1

verify: test race vet integration-test
