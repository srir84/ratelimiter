// Package storeproc writes only locals.
package storeproc

type Store struct{ Port int }

func Start() *Store {
	s := &Store{}
	s.Port = 6390
	return s
}
