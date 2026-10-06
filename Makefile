.PHONY: help build test race lint run demo clean docker
BIN := bin/forge

help:            ## list targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-10s %s\n", $$1, $$2}'
build:           ## build bin/forge
	go build -o $(BIN) ./cmd/forge
test:            ## unit + end-to-end tests
	go test ./...
race:            ## the one that matters
	go test -race ./...
lint:            ## vet + gofmt check
	go vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
run: build       ## dev server on :8080 with the fake provider
	$(BIN) serve --listen :8080 --db ./forge.db
demo: build      ## apply the examples against a running server and watch them converge
	$(BIN) apply -f examples/ds-alice.yaml
	$(BIN) apply -f examples/api-staging.yaml
	sleep 3; $(BIN) get; $(BIN) events ds-alice
docker:          ## multi-arch image (needs buildx)
	docker buildx build --platform linux/amd64,linux/arm64 -t forge:dev .
clean:
	rm -rf bin forge.db
