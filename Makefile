.PHONY: all build test lint vet fmt tidy check cover cover-check clean

all: check

build:
	go build ./...
	go build ./examples/...

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run ./...

vet:
	go vet ./...

fmt:
	gofmt -s -w .

tidy:
	go mod tidy

check: fmt tidy vet build test

cover:
	go test -coverprofile=/tmp/agentscope-cover.out ./... >/dev/null
	@printf "total ./...     : "; go tool cover -func=/tmp/agentscope-cover.out | tail -1
	go test -coverprofile=/tmp/agentscope-cover-pkg.out ./pkg/... >/dev/null
	@printf "total ./pkg/... : "; go tool cover -func=/tmp/agentscope-cover-pkg.out | tail -1

cover-check:
	go test -coverprofile=/tmp/agentscope-cover-pkg.out ./pkg/... >/dev/null
	@total=$$(go tool cover -func=/tmp/agentscope-cover-pkg.out | tail -1 | awk '{print $$NF}' | tr -d '%'); \
	echo "pkg coverage: $${total}% (floor $${COVERAGE_MIN:-70.0}%)"; \
	awk -v t="$$total" -v min="$${COVERAGE_MIN:-70.0}" 'BEGIN { if (t+0 < min+0) { print "ERROR: coverage below floor"; exit 1 } }'

clean:
	go clean -cache -testcache
