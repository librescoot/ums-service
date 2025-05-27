.PHONY: build clean build-arm build-amd64 lint test

BINARY_NAME=ums-service
BUILD_DIR=bin
LDFLAGS=-ldflags "-w -s -extldflags '-static'"

build:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/ums-service

clean:
	rm -rf $(BUILD_DIR)

build-arm:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME) ./cmd/ums-service

build-amd64:
	mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_NAME)-amd64 ./cmd/ums-service

lint:
	golangci-lint run

test:
	go test -v ./...