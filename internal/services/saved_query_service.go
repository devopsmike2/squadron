package services

import (
	"context"
	"time"
)

// SavedQueryService defines operations for managing saved Squadron QL queries.
type SavedQueryService interface {
	ListSavedQueries(ctx context.Context) ([]*SavedQuery, error)
	CreateSavedQuery(ctx context.Context, input SavedQueryInput) (*SavedQuery, error)
	UpdateSavedQuery(ctx context.Context, id string, input SavedQueryInput) (*SavedQuery, error)
	DeleteSavedQuery(ctx context.Context, id string) error
}

// SavedQuery represents a persisted Squadron QL query definition.
type SavedQuery struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Query       string    `json:"query"`
	Tags        []string  `json:"tags"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// SavedQueryInput captures user-editable fields for a saved query.
type SavedQueryInput struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Query       string   `json:"query"`
	Tags        []string `json:"tags"`
}
