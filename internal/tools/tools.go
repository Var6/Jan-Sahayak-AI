//go:build tools

// Package tools pins module dependencies that are used by more than one
// binary so that parallel work does not race on go.mod. It is never built.
package tools

import (
	_ "github.com/jackc/pgx/v5/pgxpool"
	_ "go.uber.org/goleak"
	_ "golang.org/x/crypto/argon2"
)
