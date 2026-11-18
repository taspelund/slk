// Package e2e is the Matrix backend's end-to-end test harness: a throwaway
// Synapse in Docker (scripts/matrix-dev/synapse.sh), users registered with
// its shared secret, and Peer, a second mautrix client with encryption that
// plays the other side of a conversation.
//
// Everything but this file is behind the matrixe2e build tag, so a plain
// `go test ./...` needs no Docker. Run the suite with `make matrix-e2e`, or
// start the server yourself and run the tests against it:
//
//	eval "$(scripts/matrix-dev/synapse.sh start >/dev/null && scripts/matrix-dev/synapse.sh env)"
//	go test -tags matrixe2e ./internal/matrix/...
//	scripts/matrix-dev/synapse.sh stop
//
// Without MATRIX_E2E_HOMESERVER and MATRIX_E2E_SHARED_SECRET set, Homeserver
// starts the server with the script itself (or reuses a running one) and
// leaves it running for the next run; stop it with the script.
//
// Never point these tests at a real homeserver or account.
package e2e
