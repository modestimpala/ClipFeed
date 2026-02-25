package feed

import (
	"testing"
	"time"
)

// makeModel wraps a flat node slice into an LTRModel for convenience.
func makeModel() *LTRModel { return &LTRModel{} }

func TestScoreTree_Leaf(t *testing.T) {
	nodes := []LTRTree{
		{IsLeaf: true, LeafValue: 3.14},
	}
	got := makeModel().scoreTree(nodes, []float64{0.0})
	if got != 3.14 {
		t.Errorf("scoreTree leaf = %v, want 3.14", got)
	}
}

func TestScoreTree_LeftPath(t *testing.T) {
	// features[0]=0.0 <= threshold 0.5 → take left child (index 1) → leaf 1.0
	nodes := []LTRTree{
		{IsLeaf: false, FeatureIndex: 0, Threshold: 0.5, LeftChild: 1, RightChild: 2},
		{IsLeaf: true, LeafValue: 1.0},
		{IsLeaf: true, LeafValue: 2.0},
	}
	got := makeModel().scoreTree(nodes, []float64{0.0})
	if got != 1.0 {
		t.Errorf("scoreTree left path = %v, want 1.0", got)
	}
}

func TestScoreTree_RightPath(t *testing.T) {
	// features[0]=0.9 > threshold 0.5 → take right child (index 2) → leaf 2.0
	nodes := []LTRTree{
		{IsLeaf: false, FeatureIndex: 0, Threshold: 0.5, LeftChild: 1, RightChild: 2},
		{IsLeaf: true, LeafValue: 1.0},
		{IsLeaf: true, LeafValue: 2.0},
	}
	got := makeModel().scoreTree(nodes, []float64{0.9})
	if got != 2.0 {
		t.Errorf("scoreTree right path = %v, want 2.0", got)
	}
}

func TestScoreTree_Empty(t *testing.T) {
	got := makeModel().scoreTree(nil, []float64{1.0})
	if got != 0 {
		t.Errorf("scoreTree empty = %v, want 0", got)
	}
}

func TestScoreTree_InvalidFeatureIndex(t *testing.T) {
	nodes := []LTRTree{
		{IsLeaf: false, FeatureIndex: 99, Threshold: 0.5, LeftChild: 1, RightChild: 1},
		{IsLeaf: true, LeafValue: 1.0},
	}
	got := makeModel().scoreTree(nodes, []float64{0.0})
	if got != 0 {
		t.Errorf("scoreTree bad feature index = %v, want 0", got)
	}
}

func TestScoreTree_OutOfBoundsChild(t *testing.T) {
	nodes := []LTRTree{
		{IsLeaf: false, FeatureIndex: 0, Threshold: 0.5, LeftChild: 99, RightChild: 99},
	}
	got := makeModel().scoreTree(nodes, []float64{0.0})
	if got != 0 {
		t.Errorf("scoreTree out-of-bounds child = %v, want 0", got)
	}
}

// TestScoreTree_SelfCycle verifies that a node pointing to itself (index 0→0)
// does not hang. Previously this would loop forever; now it must return within
// the step budget and yield 0.
func TestScoreTree_SelfCycle(t *testing.T) {
	nodes := []LTRTree{
		{IsLeaf: false, FeatureIndex: 0, Threshold: 0.5, LeftChild: 0, RightChild: 0},
	}
	done := make(chan float64, 1)
	go func() { done <- makeModel().scoreTree(nodes, []float64{0.0}) }()
	select {
	case got := <-done:
		if got != 0 {
			t.Errorf("scoreTree self-cycle = %v, want 0", got)
		}
	case <-time.After(time.Second):
		t.Fatal("scoreTree with self-referencing node did not return (infinite loop)")
	}
}

// TestScoreTree_TwoNodeCycle verifies that a two-node cycle (0→1→0→…) does not
// hang. Previously this WILL infinite loop; the step-budget guard fixes it.
func TestScoreTree_TwoNodeCycle(t *testing.T) {
	nodes := []LTRTree{
		{IsLeaf: false, FeatureIndex: 0, Threshold: 0.5, LeftChild: 1, RightChild: 1},
		{IsLeaf: false, FeatureIndex: 0, Threshold: 0.5, LeftChild: 0, RightChild: 0},
	}
	done := make(chan float64, 1)
	go func() { done <- makeModel().scoreTree(nodes, []float64{0.0}) }()
	select {
	case got := <-done:
		if got != 0 {
			t.Errorf("scoreTree two-node cycle = %v, want 0", got)
		}
	case <-time.After(time.Second):
		t.Fatal("scoreTree with cyclic nodes did not return (infinite loop)")
	}
}
