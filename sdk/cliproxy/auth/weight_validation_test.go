package auth

import (
	"context"
	"encoding/json"
	"testing"
)

type credentialWeightValidationStore struct {
	auths     []*Auth
	saveCount int
}

func (s *credentialWeightValidationStore) List(context.Context) ([]*Auth, error) {
	return s.auths, nil
}

func (s *credentialWeightValidationStore) Save(context.Context, *Auth) (string, error) {
	s.saveCount++
	return "", nil
}

func (s *credentialWeightValidationStore) Delete(context.Context, string) error {
	return nil
}

func TestManagerLoadSkipsInvalidCredentialWeights(t *testing.T) {
	store := &credentialWeightValidationStore{
		auths: []*Auth{
			{ID: "omitted", Provider: "test"},
			{ID: "zero", Provider: "test", Metadata: map[string]any{AttributeWeight: json.Number("0")}},
			{ID: "fraction", Provider: "test", Metadata: map[string]any{AttributeWeight: json.Number("1.5")}},
			{ID: "overflow", Provider: "test", Attributes: map[string]string{AttributeWeight: "9223372036854775808"}},
		},
	}
	manager := NewManager(store, nil, nil)

	if err := manager.Load(context.Background()); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	for _, id := range []string{"omitted", "zero"} {
		if _, ok := manager.GetByID(id); !ok {
			t.Fatalf("valid auth %q was not loaded", id)
		}
	}
	for _, id := range []string{"fraction", "overflow"} {
		if _, ok := manager.GetByID(id); ok {
			t.Fatalf("invalid auth %q remained active after Load()", id)
		}
	}
}

func TestManagerRegisterAndUpdateRejectInvalidCredentialWeights(t *testing.T) {
	store := &credentialWeightValidationStore{}
	manager := NewManager(store, nil, nil)
	ctx := context.Background()

	invalid := &Auth{
		ID:       "invalid",
		Provider: "test",
		Metadata: map[string]any{AttributeWeight: "nonnumeric"},
	}
	if _, err := manager.Register(ctx, invalid); err == nil {
		t.Fatal("Register() accepted an invalid weight")
	}
	if _, ok := manager.GetByID(invalid.ID); ok {
		t.Fatal("invalid registered auth became active")
	}
	if store.saveCount != 0 {
		t.Fatalf("invalid Register() save count = %d, want 0", store.saveCount)
	}

	valid := &Auth{
		ID:         "valid",
		Provider:   "test",
		Attributes: map[string]string{AttributeWeight: "2"},
		Metadata:   map[string]any{"type": "test"},
	}
	if _, err := manager.Register(ctx, valid); err != nil {
		t.Fatalf("Register(valid) error = %v", err)
	}
	invalidUpdate := valid.Clone()
	invalidUpdate.Attributes[AttributeWeight] = "1000001"
	if _, err := manager.Update(ctx, invalidUpdate); err == nil {
		t.Fatal("Update() accepted an invalid weight")
	}
	current, ok := manager.GetByID(valid.ID)
	if !ok || current.Attributes[AttributeWeight] != "2" {
		t.Fatalf("invalid Update() changed active auth: %#v", current)
	}
	if store.saveCount != 1 {
		t.Fatalf("save count = %d, want only the valid Register() save", store.saveCount)
	}
}
