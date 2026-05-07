package harness

import "strings"

// StepError wraps a step-level cause with a context phrase, so error
// messages from inside a smoke step include both the step's intent
// ("rensei host install") and the underlying failure.
//
// Use WrapStep rather than constructing this directly.
type StepError struct {
	context string
	cause   error
}

// Error implements error.
func (e *StepError) Error() string {
	return e.context + ": " + e.cause.Error()
}

// Unwrap surfaces the cause to errors.Is / errors.As.
func (e *StepError) Unwrap() error {
	return e.cause
}

// WrapStep returns a StepError combining a step-level context phrase with
// an underlying cause. Returns nil when err is nil so call sites can
// chain `return WrapStep("…", h.run(…))` without nil-checking.
func WrapStep(context string, err error) error {
	if err == nil {
		return nil
	}
	return &StepError{context: context, cause: err}
}

// IsUnknownSubcommand returns true when an exec error from a Cobra-based
// subprocess looks like the standard "unknown command" / "unknown flag"
// diagnostic.
//
// Used by detect-mode steps that invoke a CLI subcommand which may not
// yet be shipped — the unknown-subcommand response is the signal to
// skip the assertion cleanly rather than fail.
func IsUnknownSubcommand(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unknown command") ||
		strings.Contains(msg, "unknown flag") ||
		strings.Contains(msg, "no such command")
}

// OutputLooksLikeUnknownSubcommand returns true when captured combined
// stdout/stderr from a Cobra-based subprocess looks like the standard
// "unknown command" / "unknown flag" diagnostic.
//
// Use when the subprocess is invoked via a Run helper that captures the
// diagnostic in its returned output rather than its returned error
// (cmd.Output / Run-returning-bare-exit-status patterns).
func OutputLooksLikeUnknownSubcommand(out string) bool {
	lc := strings.ToLower(out)
	return strings.Contains(lc, "unknown command") ||
		strings.Contains(lc, "unknown flag") ||
		strings.Contains(lc, "no such command")
}
