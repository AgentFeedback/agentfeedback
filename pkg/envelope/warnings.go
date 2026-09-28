package envelope

import (
	"slices"
	"sort"
	"strings"

	"github.com/agentfeedback/agentfeedback/pkg/schema"
)

// details holds the warnings of one decode as a trie keyed by the path of
// the member they point at (unescaped reference tokens). Parse-time warnings
// point into the body as sent; inference moves content around, and with it
// the warnings: dropping those under a discarded duplicate, remapping a moved
// member's, collapsing a coerced value's onto the member. Each is a subtree
// operation, so the cost is the size of the subtree, never a scan of every
// warning; a body with a warning per member stays linear.
type details struct {
	root *node
}

type node struct {
	own      []entry // warnings at exactly this path
	children map[string]*node
}

type entry struct {
	code    string
	message string
}

func newDetails() *details { return &details{root: &node{}} }

// child returns the child for token, creating it.
func (n *node) child(token string) *node {
	if c, ok := n.children[token]; ok {
		return c
	}
	if n.children == nil {
		n.children = map[string]*node{}
	}
	c := &node{}
	n.children[token] = c
	return c
}

func (n *node) add(code, message string) {
	n.own = append(n.own, entry{code, message})
}

// at returns the node at path, creating the path.
func (d *details) at(path ...string) *node {
	n := d.root
	for _, token := range path {
		n = n.child(token)
	}
	return n
}

// lookup returns the node at path, or nil.
func (d *details) lookup(path []string) *node {
	n := d.root
	for _, token := range path {
		c, ok := n.children[token]
		if !ok {
			return nil
		}
		n = c
	}
	return n
}

func (d *details) add(path []string, code, message string) {
	d.at(path...).add(code, message)
}

// detach removes the subtree at path and returns it, or nil when there is
// nothing there. The root is never detached.
func (d *details) detach(path []string) *node {
	if len(path) == 0 {
		return nil
	}
	parent := d.lookup(path[:len(path)-1])
	if parent == nil {
		return nil
	}
	token := path[len(path)-1]
	n, ok := parent.children[token]
	if ok {
		delete(parent.children, token)
	}
	return n
}

// remap moves every warning under old (old itself included) under new,
// joining any warnings already there.
func (d *details) remap(old, new []string) {
	moved := d.detach(old)
	if moved == nil {
		return
	}
	merge(d.at(new...), moved)
}

func merge(dst, src *node) {
	dst.own = append(dst.own, src.own...)
	for token, c := range src.children {
		if existing, ok := dst.children[token]; ok {
			merge(existing, c)
		} else {
			if dst.children == nil {
				dst.children = map[string]*node{}
			}
			dst.children[token] = c
		}
	}
}

// collapse moves every warning below path onto path itself: a structured
// value became one string, so its parts no longer exist.
func (d *details) collapse(path []string) {
	n := d.lookup(path)
	if n == nil {
		return
	}
	for _, c := range n.children {
		gather(n, c)
	}
	n.children = nil
}

func gather(dst, src *node) {
	dst.own = append(dst.own, src.own...)
	for _, c := range src.children {
		gather(dst, c)
	}
}

// flatten returns every warning with its RFC 6901 pointer, in pointer order
// so that the output is deterministic.
func (d *details) flatten() []schema.Detail {
	var out []schema.Detail
	var walk func(n *node, pointer string)
	walk = func(n *node, pointer string) {
		for _, e := range n.own {
			out = append(out, schema.Detail{Code: e.code, Pointer: pointer, Message: e.message})
		}
		tokens := make([]string, 0, len(n.children))
		for token := range n.children {
			tokens = append(tokens, token)
		}
		slices.Sort(tokens)
		for _, token := range tokens {
			walk(n.children[token], pointer+"/"+schema.EscapeToken(token))
		}
	}
	walk(d.root, "")
	sort.SliceStable(out, func(i, j int) bool {
		if c := strings.Compare(out[i].Pointer, out[j].Pointer); c != 0 {
			return c < 0
		}
		return out[i].Code < out[j].Code
	})
	return out
}
