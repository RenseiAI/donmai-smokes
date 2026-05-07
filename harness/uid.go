package harness

import "os"

// Getuid returns the current process's user ID.
//
// Used by harness consumers that need to construct service-manager paths
// keyed on UID — e.g. macOS launchctl's `gui/<uid>/<label>` domain target —
// where the UID is the same one launchd assigns to the user-domain agent.
func Getuid() int {
	return os.Getuid()
}

// FileExists returns true when path exists and is a regular file.
//
// Returns false for directories, symlinks to directories, missing paths,
// and permission errors. Callers that need to distinguish those cases
// should use os.Stat directly.
func FileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}
