package main

import (
	"errors"
	"path/filepath"
)

// The Matrix backend's entry points, behind hidden flags while it is being
// built (see internal/matrix):
//
//	slk --matrix-login <account>   log in and save the device
//	slk --matrix <account>         run the TUI against that account
//
// Each account's credentials and database live in matrixAccountDir.

// matrixAccountDir is where account keeps session.json and matrix.db.
func matrixAccountDir(account string) string {
	return filepath.Join(xdgData(), "matrix", account)
}

// runMatrixLogin asks for a homeserver, user and password, logs in and
// saves the credentials.
func runMatrixLogin(account string) error {
	return errors.New("--matrix-login is not implemented yet")
}

// runMatrix runs slk against account, wired the way runDemo wires
// internal/demo.
func runMatrix(account string) error {
	return errors.New("--matrix is not implemented yet")
}
