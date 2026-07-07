BINARY_NAME=zon-cacher

.PHONY: build

build:
	go build -o $(BINARY_NAME) .
