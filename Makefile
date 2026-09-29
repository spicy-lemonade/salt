# Unit tests never start processes (enforced by internal/rules). Anything that
# runs the salt binary lives in test/e2e, behind the e2e build tag, and runs
# only through `make e2e`, which:
#   - builds salt once, here, never inside a test
#   - caps processes at the current count + E2E_EXTRA_PROCS, so a runaway
#     test fails with "resource temporarily unavailable" instead of
#     exhausting the machine
#   - sets a soft memory limit and a hard timeout
#   - sets SALT_E2E=1; the tests pass SALT_KEYSTORE=file and a temp HOME to
#     every salt they start, so the real keychain is never touched

GO_TEST_FLAGS   := -p 2 -timeout 120s
E2E_EXTRA_PROCS := 200

.PHONY: build test vet e2e check clean

build:
	go build -o bin/salt ./cmd/salt

test:
	GOMEMLIMIT=1GiB go test $(GO_TEST_FLAGS) ./...

vet:
	go vet ./...
	go vet -tags e2e ./test/e2e/...

e2e: build
	@procs=$$(ps -U "$$(id -u)" | wc -l | tr -d ' '); \
	ulimit -u $$((procs + $(E2E_EXTRA_PROCS))) && \
	SALT_E2E=1 SALT_BIN="$(CURDIR)/bin/salt" GOMEMLIMIT=1GiB \
	go test -tags e2e -p 1 -count=1 -timeout 120s ./test/e2e/...

check: vet test e2e

clean:
	rm -rf bin
