.PHONY: run build generate test

generate:
	go tool templ generate

run: generate
	go run .

build: generate
	go build -o telegram-cleanup .

test:
	go test ./...
