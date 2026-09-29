package resolve

import (
	"sync/atomic"
	"time"

	"github.com/fozagtx/coinstack/internal/model"
)

// empty is served by a Live that has not been given an index yet.
var empty = Build(nil, time.Time{}, Options{})

// Live is the resolver the service runs with: it holds the current *Index
// and lets a rebuilt one be published with Swap while other goroutines keep
// resolving. The zero value is ready to use and behaves as an empty index.
// Live implements api.Resolver.
type Live struct {
	cur atomic.Pointer[Index]
}

// Swap publishes ix as the current index. In-flight lookups finish against
// the index they started with. A nil ix resets Live to an empty index.
func (l *Live) Swap(ix *Index) {
	if ix == nil {
		ix = empty
	}
	l.cur.Store(ix)
}

// Load returns the current index; it is never nil.
func (l *Live) Load() *Index {
	if ix := l.cur.Load(); ix != nil {
		return ix
	}
	return empty
}

// Resolve resolves query against the current index; see Index.Resolve.
func (l *Live) Resolve(query string) (model.Resolution, error) { return l.Load().Resolve(query) }

// Search searches the current index; see Index.Search.
func (l *Live) Search(query string, limit int) []model.Candidate {
	return l.Load().Search(query, limit)
}

// Size returns the number of assets in the current index.
func (l *Live) Size() int { return l.Load().Size() }

// BuiltAt returns when the current index was built; the zero time until the
// first Swap.
func (l *Live) BuiltAt() time.Time { return l.Load().BuiltAt() }
