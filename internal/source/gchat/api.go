package gchat

import (
	"context"
	"io"

	chat "google.golang.org/api/chat/v1"
)

// chatAPI is the thin seam between the connector and google.golang.org/api/
// chat/v1 — just enough surface for tests to fake. Implementations pin every
// fixed listing parameter (filter, page size, ordering) so that all pages of
// one listing use identical params, as the API requires; only the page token
// varies between pages.
type chatAPI interface {
	probeSpaces(ctx context.Context) error
	listSpaces(ctx context.Context, pageToken string) (*chat.ListSpacesResponse, error)
	listMembers(ctx context.Context, space, pageToken string) (*chat.ListMembershipsResponse, error)
	listMessages(ctx context.Context, space, filter, pageToken string, showDeleted bool) (*chat.ListMessagesResponse, error)
	getMessage(ctx context.Context, name string) (*chat.Message, error)
	listSpaceEvents(ctx context.Context, space, filter, pageToken string) (*chat.ListSpaceEventsResponse, error)
	downloadMedia(ctx context.Context, resourceName string) (io.ReadCloser, error)
}

// spaceTypeFilter lists every space shape the archive covers; spaces.list
// only returns spaces the authorized user is a member of.
const spaceTypeFilter = `spaceType = "SPACE" OR spaceType = "GROUP_CHAT" OR spaceType = "DIRECT_MESSAGE"`

// listPageSize is the documented maximum for spaces, members and messages
// listings (defaults are far smaller: 100/100/25).
const listPageSize = 1000

type realAPI struct {
	svc *chat.Service
}

func (r *realAPI) probeSpaces(ctx context.Context) error {
	_, err := r.svc.Spaces.List().Filter(spaceTypeFilter).PageSize(1).Context(ctx).Do()
	return err
}

func (r *realAPI) listSpaces(ctx context.Context, pageToken string) (*chat.ListSpacesResponse, error) {
	call := r.svc.Spaces.List().Filter(spaceTypeFilter).PageSize(listPageSize).Context(ctx)
	if pageToken != "" {
		call = call.PageToken(pageToken)
	}
	return call.Do()
}

func (r *realAPI) listMembers(ctx context.Context, space, pageToken string) (*chat.ListMembershipsResponse, error) {
	call := r.svc.Spaces.Members.List(space).PageSize(listPageSize).Context(ctx)
	if pageToken != "" {
		call = call.PageToken(pageToken)
	}
	return call.Do()
}

func (r *realAPI) listMessages(ctx context.Context, space, filter, pageToken string, showDeleted bool) (*chat.ListMessagesResponse, error) {
	call := r.svc.Spaces.Messages.List(space).
		OrderBy("createTime ASC").
		PageSize(listPageSize).
		ShowDeleted(showDeleted).
		Context(ctx)
	if filter != "" {
		call = call.Filter(filter)
	}
	if pageToken != "" {
		call = call.PageToken(pageToken)
	}
	return call.Do()
}

func (r *realAPI) getMessage(ctx context.Context, name string) (*chat.Message, error) {
	return r.svc.Spaces.Messages.Get(name).Context(ctx).Do()
}

func (r *realAPI) listSpaceEvents(ctx context.Context, space, filter, pageToken string) (*chat.ListSpaceEventsResponse, error) {
	call := r.svc.Spaces.SpaceEvents.List(space).Filter(filter).PageSize(listPageSize).Context(ctx)
	if pageToken != "" {
		call = call.PageToken(pageToken)
	}
	return call.Do()
}

func (r *realAPI) downloadMedia(ctx context.Context, resourceName string) (io.ReadCloser, error) {
	resp, err := r.svc.Media.Download(resourceName).Context(ctx).Download()
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
