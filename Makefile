VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-X github.com/chichex/che/cmd.Version=$(VERSION)"

.PHONY: build install uninstall test clean release

build:
	go build $(LDFLAGS) -o bin/che .

install: build
	cp bin/che /usr/local/bin/che
ifeq ($(shell uname -s),Darwin)
	codesign --sign - --force /usr/local/bin/che
endif

uninstall:
	rm -f /usr/local/bin/che

test:
	go test ./...

clean:
	rm -rf bin/ dist/

release:
	goreleaser release --clean
