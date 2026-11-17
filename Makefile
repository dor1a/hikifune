GO_IMAGE ?= golang:1.26-alpine
VERSION  ?= $(shell git describe --tags --match 'v*' --always --dirty 2>/dev/null || echo dev)
DOCKER_GO = docker run --rm \
	-v hikifune-gomod:/go/pkg/mod -v hikifune-gocache:/root/.cache/go-build \
	-v "$(CURDIR):/src" -w /src $(GO_IMAGE)

.PHONY: test vet fmt fmt-check tidy image

test:
	$(DOCKER_GO) go test ./...

vet:
	$(DOCKER_GO) go vet ./...

fmt:
	$(DOCKER_GO) gofmt -w .

fmt-check:
	$(DOCKER_GO) sh -c 'test -z "$$(gofmt -l .)"'

tidy:
	$(DOCKER_GO) go mod tidy

image:
	docker build --target server --build-arg VERSION=$(VERSION) -t hikifune:$(VERSION) .
	docker build --target agent --build-arg VERSION=$(VERSION) -t hikifune-agent:$(VERSION) .
