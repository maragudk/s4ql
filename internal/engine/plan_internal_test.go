package engine

import (
	"testing"

	"maragu.dev/is"
)

// otherPlan and otherNode are a plan and a node of types that no statement gives.
type otherPlan struct{}
type otherNode struct{}

func (otherPlan) plan() {}
func (otherNode) node() {}

func TestFormat_unknown(t *testing.T) {
	t.Run("should panic for a plan or a node of a type that it does not know", func(t *testing.T) {
		for _, p := range []Plan{otherPlan{}, &Query{Root: otherNode{}}} {
			func() {
				defer func() { is.True(t, recover() != nil) }()
				Format(p)
			}()
		}
	})
}
