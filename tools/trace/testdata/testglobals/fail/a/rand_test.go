package a

import "math/rand/v2"

// math/rand/v2's generators advance their state on every draw: a package-level one is
// shared state, by its type (one top-level pointer removed) or by its constructor.
var (
	rng  = rand.New(rand.NewPCG(1, 2))  // want
	pcg  rand.PCG                       // want
	cha  = rand.NewChaCha8([32]byte{})  // want
	zipf = rand.NewZipf(rng, 1.1, 1, 9) // want
	rngp *rand.Rand                     // want
	src  rand.Source                    // passes: an interface type holds no generator itself
)
