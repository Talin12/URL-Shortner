// Package links creates short links: an ID from the allocator, a code from the
// Feistel codec, and a row in the store.
package links

import (
	"context"
	"fmt"

	"github.com/Talin12/URL-Shortner/internal/shortcode"
	"github.com/Talin12/URL-Shortner/internal/store"
)

// IDSource hands out link IDs.
type IDSource interface {
	Next(ctx context.Context) (uint64, error)
}

// LinkWriter persists a link whose ID and code are already decided.
type LinkWriter interface {
	InsertLink(ctx context.Context, id uint64, code, destination string) (store.Link, error)
}

// Creator turns a destination URL into a stored link.
//
// The two halves are deliberately separate concerns that happen to share a
// solution: the allocator removes coordination from creation, and the codec
// removes the enumerability that block-allocated sequential IDs would
// otherwise hand out for free.
type Creator struct {
	ids   IDSource
	codec *shortcode.Codec
	store LinkWriter
}

// New wires a creator.
func New(ids IDSource, codec *shortcode.Codec, s LinkWriter) *Creator {
	return &Creator{ids: ids, codec: codec, store: s}
}

// Create allocates an ID, derives its code, and inserts the row.
//
// There is no collision check and no retry loop, because there is nothing to
// collide: IDs are unique by allocation and the codec is a bijection, so
// distinct IDs cannot produce the same code.
func (c *Creator) Create(ctx context.Context, destination string) (store.Link, error) {
	id, err := c.ids.Next(ctx)
	if err != nil {
		return store.Link{}, err
	}
	if id > shortcode.MaxID {
		return store.Link{}, fmt.Errorf("links: id %d exceeds the codec domain; the keyspace is full", id)
	}
	return c.store.InsertLink(ctx, id, c.codec.Encode(id), destination)
}
