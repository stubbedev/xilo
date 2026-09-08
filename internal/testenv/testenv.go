// Package testenv holds one rule: a test that cannot run must not go quiet.
//
// Several suites here need something the machine may not have (nix on PATH, a
// PostgreSQL server, mkfifo) and skip when it is missing, which is right on a
// laptop and wrong in CI. The differential test that pins the NAR byte layout
// against `nix-store --dump` skipped in CI for as long as it existed, because
// the job that ran `go test` never installed nix, and a skip prints nothing
// without -v. The suite looked green while the repo's most safety-critical
// invariant went unchecked.
//
// So CI declares what it promised to provide by setting XILO_STRICT_TESTS,
// and a prerequisite missing under that flag is a failure rather than a skip.
// Nothing here changes how the tests behave on a developer machine.
package testenv

import (
	"os"
	"testing"
)

// StrictVar is the environment variable CI sets to say "everything these tests
// need is installed; if something is missing that is my bug, not a reason to
// stop testing".
const StrictVar = "XILO_STRICT_TESTS"

// Strict reports whether this run promised to satisfy every prerequisite.
func Strict() bool { return os.Getenv(StrictVar) != "" }

// Service gates a test on a prerequisite only some jobs provide: a database, a
// MinIO. It skips when the thing is absent, unless requireVar is set, which is
// how the one job that does provide it says so and turns a missing service
// into a failure rather than a silently green run.
//
// StrictVar deliberately does not cover these. The general test job has nix
// but no service containers, so making its skips fatal would only mean nobody
// sets StrictVar at all. (internal/store reads XILO_TEST_BACKEND as its own
// require signal, for the same reason and to the same effect.)
func Service(tb testing.TB, requireVar, what string) {
	tb.Helper()
	if os.Getenv(requireVar) != "" {
		tb.Fatalf("%s is unavailable but %s is set, so this job was supposed to provide it", what, requireVar)
	}
	tb.Skipf("%s unavailable", what)
}

// Need skips the test when a prerequisite is absent, or fails it when this run
// promised to have it. `what` names the missing thing the way an operator
// would fix it ("nix-store on PATH"), and `why` carries the underlying error
// or detail, empty when there is none.
func Need(tb testing.TB, what, why string) {
	tb.Helper()
	msg := what + " unavailable"
	if why != "" {
		msg += ": " + why
	}
	if Strict() {
		tb.Fatalf("%s (%s is set, so this run was supposed to have it)", msg, StrictVar)
	}
	tb.Skip(msg)
}
