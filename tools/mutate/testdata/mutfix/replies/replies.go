// Package replies is the fixture of TestMutate_RepliesFile: its test stands for an L2 test,
// which records each reply it gets.
package replies

// Code returns the reply code for n: 1 when n is positive, else 0.
func Code(n int) int {
	if n > 0 {
		return 1
	}
	return 0
}
