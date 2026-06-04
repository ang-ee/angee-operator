.PHONY: init build build-cli build-operator generate check-generated schema check-schema test fmt vet check clean

VERSION ?= dev
LDFLAGS := -s -w -X github.com/ang-ee/angee-operator/internal/cli.Version=$(VERSION) -X github.com/ang-ee/angee-operator/internal/operator.Version=$(VERSION)

build: build-cli build-operator

install: build
	ANGEE_DIST_DIR="$(CURDIR)/dist" sh scripts/install.sh

build-cli:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o dist/angee ./cmd/angee

build-operator:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o dist/angee-operator ./cmd/operator

generate:
	go generate ./internal/operator

check-generated: generate
	git diff --exit-code -- internal/operator/gql

schema:
	go run ./cmd/schema -o docs/public/angee.schema.json

check-schema: schema
	git diff --exit-code -- docs/public/angee.schema.json

test:
	go test -v -race ./...

fmt:
	gofmt -w .

vet:
	go vet ./...

check: fmt vet test

clean:
	rm -rf dist coverage.out
