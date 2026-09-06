package links

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/Talin12/URL-Shortner/internal/idgen"
	"github.com/Talin12/URL-Shortner/internal/shortcode"
	"github.com/Talin12/URL-Shortner/internal/store"
)

type fakeWriter struct {
	mu    sync.Mutex
	codes map[string]uint64
	err   error
}

func (f *fakeWriter) InsertLink(_ context.Context, id uint64, code, destination string) (store.Link, error) {
	if f.err != nil {
		return store.Link{}, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.codes == nil {
		f.codes = map[string]uint64{}
	}
	if prev, dup := f.codes[code]; dup {
		return store.Link{}, errors.New("duplicate code from ids " + string(rune(prev)) + " and " + string(rune(id)))
	}
	f.codes[code] = id
	return store.Link{ID: id, Code: code, Destination: destination}, nil
}

func newTestCreator(w *fakeWriter, blockSize uint64) *Creator {
	var next uint64
	var mu sync.Mutex
	claim := func(_ context.Context, size uint64) (uint64, error) {
		mu.Lock()
		defer mu.Unlock()
		start := next
		next += size
		return start, nil
	}
	return New(idgen.New(claim, blockSize), shortcode.NewCodec(42), w)
}

func TestCreateProducesDecodableCodes(t *testing.T) {
	w := &fakeWriter{}
	codec := shortcode.NewCodec(42)
	c := New(idgen.New(func(context.Context, uint64) (uint64, error) { return 500, nil }, 100), codec, w)

	link, err := c.Create(context.Background(), "https://example.com")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	id, err := codec.Decode(link.Code)
	if err != nil {
		t.Fatalf("Decode(%q): %v", link.Code, err)
	}
	if id != link.ID {
		t.Errorf("code %q decodes to %d, want the link's id %d", link.Code, id, link.ID)
	}
}

// Uniqueness has to hold with no collision check anywhere in the path.
func TestConcurrentCreatesProduceUniqueCodes(t *testing.T) {
	w := &fakeWriter{}
	c := newTestCreator(w, 64)

	const goroutines, each = 40, 100
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*each)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := c.Create(context.Background(), "https://example.com"); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent Create: %v", err)
	}
	if len(w.codes) != goroutines*each {
		t.Errorf("stored %d unique codes, want %d", len(w.codes), goroutines*each)
	}
}

func TestSequentialCreatesAreNotEnumerable(t *testing.T) {
	w := &fakeWriter{}
	c := newTestCreator(w, 1000)

	var codes []string
	for i := 0; i < 20; i++ {
		link, err := c.Create(context.Background(), "https://example.com")
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		codes = append(codes, link.Code)
	}

	// Consecutive IDs must not give codes a scanner could walk. Sharing a
	// prefix is the giveaway, since base62 of adjacent integers differs only
	// in the last character.
	var sharedPrefix int
	for i := 1; i < len(codes); i++ {
		if len(codes[i]) == len(codes[i-1]) && len(codes[i]) > 1 &&
			codes[i][:len(codes[i])-1] == codes[i-1][:len(codes[i-1])-1] {
			sharedPrefix++
		}
	}
	if sharedPrefix > 1 {
		t.Errorf("%d consecutive code pairs shared a prefix; codes look enumerable: %v", sharedPrefix, codes)
	}
}

func TestCreatePropagatesAllocatorFailure(t *testing.T) {
	want := errors.New("postgres is down")
	c := New(idgen.New(func(context.Context, uint64) (uint64, error) { return 0, want }, 10), shortcode.NewCodec(1), &fakeWriter{})

	if _, err := c.Create(context.Background(), "https://example.com"); err == nil {
		t.Error("Create succeeded despite a failing allocator")
	}
}

func TestCreateRejectsIDsPastTheCodecDomain(t *testing.T) {
	c := New(idgen.New(func(context.Context, uint64) (uint64, error) { return shortcode.MaxID, nil }, 10), shortcode.NewCodec(1), &fakeWriter{})

	// First ID is exactly MaxID and is fine; the next one is out of domain.
	if _, err := c.Create(context.Background(), "https://example.com"); err != nil {
		t.Fatalf("Create at MaxID: %v", err)
	}
	if _, err := c.Create(context.Background(), "https://example.com"); err == nil {
		t.Error("Create accepted an id past the codec domain instead of reporting a full keyspace")
	}
}
