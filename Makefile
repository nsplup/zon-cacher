BINARY_NAME=zon-cacher

.PHONY: build

build:
	go build -trimpath -o $(BINARY_NAME) .
