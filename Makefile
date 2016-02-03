
TARGETS:=$(wildcard ./cmd/*)
GOCMD:=$(if $(shell which godep),godep go,go)
VERSION:=$(shell git describe --always)
PACKAGES:=$(shell go list ./... | grep -v /vendor/)

all: $(TARGETS)

test:
	$(GOCMD) test $(PACKAGES)

clean:
	rm -rf ./bin

./bin:
	mkdir -p ./bin

# go will track changes in dependencies, so the makefile does not need to do
# that. That means we always compile everything here.
# Need to include initial "./" in path so go knows it is a relative package path.
$(TARGETS): ./bin
	$(GOCMD) build -ldflags "-X github.com/ndlib/bendo/server.Version=$(VERSION)" \
		-o ./bin/$(notdir $@) ./$@

.PHONY: $(TARGETS)
