.PHONY: help build test race lint run demo clean docker
BIN := bin/berth

help:            ## list targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  %-10s %s\n", $$1, $$2}'
build:           ## build bin/berth
	go build -o $(BIN) ./cmd/berth
test:            ## unit + end-to-end tests
	go test ./...
race:            ## the one that matters
	go test -race ./...
lint:            ## vet + gofmt check
	go vet ./...
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)
run: build       ## dev server on :8080 (API) and :8081 (gateway), open: no tokens
	$(BIN) serve --listen :8080 --gateway-listen :8081 --db ./berth.db
demo: build      ## apply the examples against a running server; table, events, costs, a request through the gateway
	$(BIN) apply -f examples/ds-alice.yaml
	$(BIN) apply -f examples/api-staging.yaml
	sleep 3; $(BIN) get; $(BIN) events ds-alice; $(BIN) costs
	curl -s -H 'Host: api-staging.example.com' localhost:8081/; echo
docker:          ## multi-arch image (needs buildx)
	docker buildx build --platform linux/amd64,linux/arm64 -t berth:dev .
clean:
	rm -rf bin berth.db berth.db.fake-inventory.json
