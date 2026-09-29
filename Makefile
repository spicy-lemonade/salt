# Unit tests never start processes (enforced by internal/rules). Anything that
# runs the salt binary lives in test/e2e and only runs inside a capped
# container, so a runaway test cannot exhaust the host.

GO_TEST_FLAGS := -p 2 -timeout 120s
E2E_IMAGE     := salt-e2e
E2E_LIMITS    := --memory=2g --memory-swap=2g --pids-limit=256 --cpus=2

.PHONY: build test vet e2e check clean

build:
	go build -o bin/salt ./cmd/salt

test:
	GOMEMLIMIT=1GiB go test $(GO_TEST_FLAGS) ./...

vet:
	go vet ./...
	go vet -tags e2e ./test/e2e/...

e2e:
	docker build -f test/e2e/Dockerfile -t $(E2E_IMAGE) .
	docker run --rm $(E2E_LIMITS) --network=none $(E2E_IMAGE)

check: vet test e2e

clean:
	rm -rf bin
