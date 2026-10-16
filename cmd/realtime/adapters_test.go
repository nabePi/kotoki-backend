package main

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
)

type fakePrompts struct {
	pv  content.PromptVersion
	err error
}

func (f fakePrompts) GetPublishedPromptVersion(context.Context, content.PromptKey) (content.PromptVersion, error) {
	return f.pv, f.err
}

func TestOpeningPromptSourceAdapter(t *testing.T) {
	ctx := context.Background()
	id := uuid.New()
	got, err := openingPromptSource{fakePrompts{pv: content.PromptVersion{ID: id}}}.PublishedPromptVersionID(ctx, "OPENING")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, id, *got)

	got, err = openingPromptSource{fakePrompts{err: content.ErrNotFound}}.PublishedPromptVersionID(ctx, "OPENING")
	require.NoError(t, err, "no PUBLISHED version is not an error")
	require.Nil(t, got)

	_, err = openingPromptSource{fakePrompts{err: context.DeadlineExceeded}}.PublishedPromptVersionID(ctx, "OPENING")
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
