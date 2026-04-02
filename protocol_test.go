package main

import (
	"testing"
)

func TestNextID_Increments(t *testing.T) {
	id1 := nextID()
	id2 := nextID()
	if id2 != id1+1 {
		t.Errorf("expected sequential IDs, got %d and %d", id1, id2)
	}
	if id1 < 1 {
		t.Errorf("expected positive ID, got %d", id1)
	}
}
