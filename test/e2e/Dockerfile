# e2e test image. Run through `make e2e`, which applies memory and pid limits.
FROM golang:1.26-bookworm
RUN apt-get update \
 && apt-get install -y --no-install-recommends git sqlite3 \
 && rm -rf /var/lib/apt/lists/*
RUN useradd -m tester
USER tester
WORKDIR /src
COPY --chown=tester go.mod go.sum* ./
RUN go mod download
COPY --chown=tester . .
RUN go build -o /home/tester/bin/salt ./cmd/salt
ENV SALT_BIN=/home/tester/bin/salt SALT_E2E_CONTAINER=1 GOMEMLIMIT=1GiB
CMD ["go", "test", "-tags", "e2e", "-p", "1", "-timeout", "120s", "./test/e2e/..."]
