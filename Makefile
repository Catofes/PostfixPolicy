.PHONY: all check

all:
	mkdir -p build
	env CGO_ENABLED=0 go build -o build/policy .

check:
	go test -race ./...
	go vet ./...
