.PHONY: build test clean

.DEFAULT_GOAL := build

build: go.sum
	go build -o main

test: go.sum
	go test

clean:
	rm main
