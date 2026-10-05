// Package a declares the package-level variables its tests write.
package a

type (
	box  struct{ n int }
	ints []int
	// byLen sorts names by length (sort.Interface).
	byLen []string
	// vec is a generic slice type: a conversion to an instance of it keeps the storage.
	vec[T any] []T
	tcase      struct {
		name string
		slow bool
	}
)

func isSlow(c tcase) bool { return c.slow }

func (b byLen) Len() int           { return len(b) }
func (b byLen) Less(i, j int) bool { return len(b[i]) < len(b[j]) }
func (b byLen) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }

var (
	counter  int
	cfg      struct{ n int }
	table    = map[string]int{}
	list     = []int{0}
	ptr      *int
	pbox         = &box{}
	boxed    any = &box{}
	v        box
	names    = []string{"b", "a"}
	floats   = []float64{2, 1}
	events   = make(chan string, 4) // declared outside the checked files: only its uses count
	shared   = []int{1, 2, 3}
	allCases = []tcase{{"a", false}, {"b", true}}
)
