package server

import (
	"errors"
	"testing"

	"bookwatch/internal/provider"
)

// fakeCoverProvider stubs the OpenLibrary provider for sourceCoverURL: a work
// either carries its own cover (WorkByID) or falls back to an edition cover
// (WorkEditions). Only the methods sourceCoverURL touches are meaningful.
type fakeCoverProvider struct {
	work     provider.Candidate
	workErr  error
	editions []provider.Edition
}

func (f *fakeCoverProvider) WorkByID(id string) (provider.Candidate, error) {
	if f.workErr != nil {
		return provider.Candidate{}, f.workErr
	}
	return f.work, nil
}
func (f *fakeCoverProvider) WorkEditions(id string) ([]provider.Edition, error) {
	return f.editions, nil
}
func (f *fakeCoverProvider) SearchByTitle(string) ([]provider.Candidate, error) { return nil, nil }
func (f *fakeCoverProvider) AuthorSearch(string) ([]provider.Author, error)     { return nil, nil }
func (f *fakeCoverProvider) AuthorWorks(string) ([]provider.Work, error)        { return nil, nil }
func (f *fakeCoverProvider) WorkDetail(string) (provider.Work, error)           { return provider.Work{}, nil }

func TestSourceCoverURL_OpenLibrary(t *testing.T) {
	const olLink = "https://openlibrary.org/works/OL4278593W"
	tests := []struct {
		name string
		p    *fakeCoverProvider
		url  string
		want string
	}{
		{
			name: "work cover",
			p:    &fakeCoverProvider{work: provider.Candidate{CoverURL: "https://covers.openlibrary.org/b/id/123-L.jpg"}},
			url:  olLink,
			want: "https://covers.openlibrary.org/b/id/123-L.jpg",
		},
		{
			name: "falls back to first edition cover when work has none",
			p: &fakeCoverProvider{
				work:     provider.Candidate{CoverURL: ""},
				editions: []provider.Edition{{CoverURL: ""}, {CoverURL: "https://covers.openlibrary.org/b/id/456-L.jpg"}},
			},
			url:  olLink,
			want: "https://covers.openlibrary.org/b/id/456-L.jpg",
		},
		{
			name: "falls back to editions when WorkByID errors",
			p: &fakeCoverProvider{
				workErr:  errors.New("boom"),
				editions: []provider.Edition{{CoverURL: "https://covers.openlibrary.org/b/id/789-L.jpg"}},
			},
			url:  olLink,
			want: "https://covers.openlibrary.org/b/id/789-L.jpg",
		},
		{
			name: "no cover anywhere",
			p:    &fakeCoverProvider{work: provider.Candidate{}},
			url:  olLink,
			want: "",
		},
		{
			name: "non-OL, non-LC link",
			p:    &fakeCoverProvider{work: provider.Candidate{CoverURL: "x"}},
			url:  "https://example.com/foo",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{ol: tt.p}
			if got := s.sourceCoverURL(tt.url); got != tt.want {
				t.Errorf("sourceCoverURL(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}
