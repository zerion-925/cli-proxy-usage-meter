# Run from the repository root. Keep builds conservative on small proxy hosts.
export GOMAXPROCS := 2
export CGO_ENABLED := 1

.PHONY: build test race vet runtime runtime-race verify
build:
	mkdir -p build
	go build -p 2 -buildmode=c-shared -o build/client-usage-meter.so ./cmd/usage-meter

test:
	go test -p 2 ./...

race:
	go test -p 2 -race ./...

vet:
	go vet -p 2 ./...

runtime: build
	go test -count=1 -p 2 -tags integration -run TestOfficialRuntime -v ./integration

runtime-race: build
	go test -count=1 -p 2 -race -tags integration -run TestOfficialRuntime -v ./integration

# Recursive recipes keep verification ordered even if invoked with make -j.
verify:
	$(MAKE) test
	$(MAKE) race
	$(MAKE) vet
	$(MAKE) runtime
