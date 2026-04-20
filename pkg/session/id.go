package session

import (
	"time"

	"github.com/dustinkirkland/golang-petname"
)

// idTimeLayout is the timestamp prefix layout for session IDs. ISO-8601 with
// colons replaced by dashes so the full ID is filesystem-safe on every
// platform.
const idTimeLayout = "2006-01-02T15-04-05Z"

// NewID returns a session ID of the form
// "YYYY-MM-DDTHH-MM-SSZ-<adverb>-<adjective>-<noun>", e.g.
// "2026-04-19T17-02-14Z-fizzy-jingling-quokka". The petname provides ~10^9
// combinations so two parallel runs in the same second are extremely unlikely
// to collide; callers that need an absolute guarantee retry on dir-exists.
func NewID(now time.Time) string {
	return now.UTC().Format(idTimeLayout) + "-" + petname.Generate(3, "-")
}
