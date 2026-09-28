package spool

// SetAfterMoveStep lets a test in package spool_test stop a process
// between the renames of Move.
func SetAfterMoveStep(f func(step string)) {
	afterMoveStep = f
}
