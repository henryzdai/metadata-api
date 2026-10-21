package memory

import (
	"container/heap"
	"sort"
)

type candidates[T any] struct {
	items []T
	limit int
	id    func(T) string
}

func newCandidates[T any](limit int, id func(T) string) *candidates[T] {
	return &candidates[T]{items: make([]T, 0, limit), limit: limit, id: id}
}

func (c *candidates[T]) Len() int           { return len(c.items) }
func (c *candidates[T]) Less(i, j int) bool { return c.id(c.items[i]) > c.id(c.items[j]) }
func (c *candidates[T]) Swap(i, j int)      { c.items[i], c.items[j] = c.items[j], c.items[i] }
func (c *candidates[T]) Push(value any)     { c.items = append(c.items, value.(T)) }
func (c *candidates[T]) Pop() any {
	last := c.items[len(c.items)-1]
	c.items = c.items[:len(c.items)-1]
	return last
}

func (c *candidates[T]) add(record T) {
	if len(c.items) < c.limit {
		heap.Push(c, record)
	} else if c.id(record) < c.id(c.items[0]) {
		c.items[0] = record
		heap.Fix(c, 0)
	}
}

func (c *candidates[T]) sorted() []T {
	sort.Slice(c.items, func(i, j int) bool { return c.id(c.items[i]) < c.id(c.items[j]) })
	return c.items
}
