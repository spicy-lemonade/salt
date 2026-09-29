package seal

// MaxIndexEntriesForTest sets MaxIndexEntries and returns the old value.
func MaxIndexEntriesForTest(n int) int {
	old := MaxIndexEntries
	MaxIndexEntries = n
	return old
}
