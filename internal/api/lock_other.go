//go:build !unix

package api

// lockDataDir is a no-op where flock is unavailable.
func lockDataDir(string) (func(), error) {
	return func() {}, nil
}
