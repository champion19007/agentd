// Package migrations carries the SQL schema as embedded files.
//
// The migrations ship inside the binary because Agentd is delivered as a
// single static executable: an operator who copies the binary onto a host has,
// by that act, everything needed to create or upgrade a database. There is no
// second artefact to keep in step with it.
package migrations

import "embed"

// FS holds every migration, named <version>_<description>.sql with a
// zero-padded four-digit version. Files are applied in filename order, which
// for that naming is also version order.
//
//go:embed *.sql
var FS embed.FS
