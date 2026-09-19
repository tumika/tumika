package tokencustody

import (
	"context"
	"testing"
)

func TestNoopStoresNothingAndSucceeds(t *testing.T) {
	if err := NewNoop().Store(context.Background(), "tk_secret"); err != nil {
		t.Fatalf("Store: %v", err)
	}
}

func TestNewReturnsAUsableCustodian(t *testing.T) {
	if New() == nil {
		t.Fatal("New returned nil")
	}
}
