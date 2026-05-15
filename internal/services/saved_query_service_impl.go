package services

import (
    "context"
    "fmt"
    "strings"
    "time"

    "github.com/google/uuid"
    "go.uber.org/zap"

    "github.com/devopsmike2/squadron/internal/storage/applicationstore"
)

type savedQueryService struct {
    appStore applicationstore.ApplicationStore
    logger   *zap.Logger
}

// NewSavedQueryService wires a SavedQueryService backed by the application store.
func NewSavedQueryService(appStore applicationstore.ApplicationStore, logger *zap.Logger) SavedQueryService {
    return &savedQueryService{appStore: appStore, logger: logger}
}

func (s *savedQueryService) ListSavedQueries(ctx context.Context) ([]*SavedQuery, error) {
    stored, err := s.appStore.ListSavedQueries(ctx)
    if err != nil {
        return nil, err
    }

    results := make([]*SavedQuery, len(stored))
    for i, sq := range stored {
        results[i] = &SavedQuery{
            ID:          sq.ID,
            Name:        sq.Name,
            Description: sq.Description,
            Query:       sq.Query,
            Tags:        append([]string{}, sq.Tags...),
            CreatedAt:   sq.CreatedAt,
            UpdatedAt:   sq.UpdatedAt,
        }
    }
    return results, nil
}

func (s *savedQueryService) CreateSavedQuery(ctx context.Context, input SavedQueryInput) (*SavedQuery, error) {
    if err := validateSavedQueryInput(input); err != nil {
        return nil, err
    }

    now := time.Now()
    record := &applicationstore.SavedQuery{
        ID:          uuid.New().String(),
        Name:        strings.TrimSpace(input.Name),
        Description: strings.TrimSpace(input.Description),
        Query:       strings.TrimSpace(input.Query),
        Tags:        append([]string{}, input.Tags...),
        CreatedAt:   now,
        UpdatedAt:   now,
    }

    if err := s.appStore.CreateSavedQuery(ctx, record); err != nil {
        return nil, err
    }

    return &SavedQuery{
        ID:          record.ID,
        Name:        record.Name,
        Description: record.Description,
        Query:       record.Query,
        Tags:        append([]string{}, record.Tags...),
        CreatedAt:   record.CreatedAt,
        UpdatedAt:   record.UpdatedAt,
    }, nil
}

func (s *savedQueryService) UpdateSavedQuery(ctx context.Context, id string, input SavedQueryInput) (*SavedQuery, error) {
    if id == "" {
        return nil, fmt.Errorf("saved query id is required")
    }
    if err := validateSavedQueryInput(input); err != nil {
        return nil, err
    }

    existing, err := s.appStore.GetSavedQuery(ctx, id)
    if err != nil {
        return nil, err
    }
    if existing == nil {
        return nil, fmt.Errorf("saved query not found: %s", id)
    }

    now := time.Now()
    record := &applicationstore.SavedQuery{
        ID:          id,
        Name:        strings.TrimSpace(input.Name),
        Description: strings.TrimSpace(input.Description),
        Query:       strings.TrimSpace(input.Query),
        Tags:        append([]string{}, input.Tags...),
        CreatedAt:   existing.CreatedAt,
        UpdatedAt:   now,
    }

    if err := s.appStore.UpdateSavedQuery(ctx, record); err != nil {
        return nil, err
    }

    return &SavedQuery{
        ID:          record.ID,
        Name:        record.Name,
        Description: record.Description,
        Query:       record.Query,
        Tags:        append([]string{}, record.Tags...),
        CreatedAt:   record.CreatedAt,
        UpdatedAt:   record.UpdatedAt,
    }, nil
}

func (s *savedQueryService) DeleteSavedQuery(ctx context.Context, id string) error {
    if id == "" {
        return fmt.Errorf("saved query id is required")
    }
    return s.appStore.DeleteSavedQuery(ctx, id)
}

func validateSavedQueryInput(input SavedQueryInput) error {
    if strings.TrimSpace(input.Name) == "" {
        return fmt.Errorf("name is required")
    }
    if strings.TrimSpace(input.Query) == "" {
        return fmt.Errorf("query is required")
    }
    return nil
}
