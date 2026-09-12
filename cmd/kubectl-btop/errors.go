package main

// usageError marks a flag-parsing, Args, or Options.Validate failure so run
// can map it to exit code 2 instead of the general-failure exit code 1.
type usageError struct {
	err error
}

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }
