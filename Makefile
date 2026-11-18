.PHONY: build install test lint run clean demo-gifs

BINARY=slk
BUILD_DIR=bin

build:
	go build -ldflags="-s -w" -trimpath -o $(BUILD_DIR)/$(BINARY) ./cmd/slk

install:
	go install -ldflags="-s -w" -trimpath ./cmd/slk

test:
	go test ./... -v -race

lint:
	golangci-lint run ./...

run: build
	./$(BUILD_DIR)/$(BINARY)

clean:
	rm -rf $(BUILD_DIR)

# Renders docs/assets/demo/*.gif from their VHS tapes (settings.tape is
# shared, not rendered). Needs vhs: run inside `nix develop`, or see
# https://github.com/charmbracelet/vhs. Not run in CI.
demo-gifs: build
	@command -v vhs >/dev/null || { echo "vhs not found: run 'nix develop' or install charmbracelet/vhs"; exit 1; }
	@for tape in docs/assets/demo/*.tape; do \
		case $$tape in */settings.tape) continue;; esac; \
		echo "vhs $$tape"; vhs $$tape || exit 1; \
	done

# End-to-end tests of the Matrix backend (internal/matrix, build tag
# matrixe2e) against a throwaway Synapse in Docker, which this starts and
# removes again. See scripts/matrix-dev/synapse.sh.
.PHONY: matrix-e2e
matrix-e2e:
	@scripts/matrix-dev/synapse.sh start >/dev/null || { scripts/matrix-dev/synapse.sh stop; exit 1; }
	@eval "$$(scripts/matrix-dev/synapse.sh env)"; \
		go test -tags matrixe2e -count=1 ./internal/matrix/...; \
		status=$$?; scripts/matrix-dev/synapse.sh stop; exit $$status
