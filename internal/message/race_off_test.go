//go:build !race

package message

// isRaceBuild tells whether the tests run under the race detector.
const isRaceBuild = false
