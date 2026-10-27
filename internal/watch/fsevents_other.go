//go:build !darwin

package watch

import "errors"

// Native returns the platform's System; only macOS has one.
func Native() (System, error) { return nil, errors.New("watch: file system events need macOS") }
