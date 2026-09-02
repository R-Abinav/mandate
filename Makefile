.PHONY: run build test check lint-check lint-fix format tidy clean

run:
	go run cmd/<>

build: 
	go build -o bin/<> cmd/<>

test:
	go test ./...

lint-check:
	golangci-lint run ./...

lint-fix:
	golangci-lint run --fix ./... 

format: 
	gofmt -w .
	goimports -w .

check:
	make format
	make lint-fix
	make lint-check
	make test

tidy:
	go mod tidy
	
clean:
	rm -rf bin/

migrate-up:
	@export $$(grep 'DATABASE_URL' .env | xargs) && \
	migrate -path migrations \
		-database "$$DATABASE_URL" \
		up

migrate-down:
	@export $$(grep 'DATABASE_URL' .env | xargs) && \
	migrate -path migrations \
		-database "$$DATABASE_URL" \
		down 1

migrate-drop:
	@export $$(grep 'DATABASE_URL' .env | xargs) && \
	migrate -path migrations \
		-database "$$DATABASE_URL" \
		drop

migrate-version:
	@export $$(grep 'DATABASE_URL' .env | xargs) && \
	migrate -path migrations \
		-database "$$DATABASE_URL" \
		version

migrate-test-up:
	@export $$(grep 'DATABASE_URL_TEST' .env | xargs) && \
	migrate -path migrations \
		-database "$$DATABASE_URL_TEST" \
		up

migrate-test-down:
	@export $$(grep 'DATABASE_URL_TEST' .env | xargs) && \
	migrate -path migrations \
		-database "$$DATABASE_URL_TEST" \
		down 1

migrate-test-drop:
	@export $$(grep 'DATABASE_URL_TEST' .env | xargs) && \
	migrate -path migrations \
		-database "$$DATABASE_URL_TEST" \
		drop

migrate-test-version:
	@export $$(grep 'DATABASE_URL_TEST' .env | xargs) && \
	migrate -path migrations \
		-database "$$DATABASE_URL_TEST" \
		version
