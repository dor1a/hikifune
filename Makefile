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

E2E       = docker compose -f deploy/compose/compose.e2e.yaml
E2E_HOSTS = docker24 docker29

.PHONY: e2e e2e-down

# Integration tests against throwaway Docker-in-Docker daemons, never the
# Docker that runs them.
e2e:
	$(E2E) up -d --wait
	for h in $(E2E_HOSTS); do \
		$(E2E) exec -T $$h sh -s < deploy/compose/e2e/containers.sh || exit 1; \
		$(E2E) exec -T $$h sh -s < deploy/compose/e2e/update.sh || exit 1; \
	done
	docker run --rm --network hikifune-e2e_default \
		-v hikifune-gomod:/go/pkg/mod -v hikifune-gocache:/root/.cache/go-build \
		-v "$(CURDIR):/src" -w /src \
		-e HIKIFUNE_E2E_HOSTS="$(foreach h,$(E2E_HOSTS),tcp://$(h):2375)" \
		$(GO_IMAGE) sh -c 'go test -tags e2e -count=1 -v -run E2E ./internal/... && \
			for h in $$HIKIFUNE_E2E_HOSTS; do HIKIFUNE_DOCKER_HOST=$$h go run ./cmd/hikifune-agent inventory > /dev/null || exit 1; done && \
			echo "agent inventory ok on $$HIKIFUNE_E2E_HOSTS"'

e2e-down:
	$(E2E) down -v
