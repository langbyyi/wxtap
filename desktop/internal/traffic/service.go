package traffic

import (
	"context"
	"fmt"
)

// DefaultPageLimit is the contract page size for traffic.list. The only
// ceiling is MaxListLimit, applied in the repository where every caller of the
// store passes through it.
const DefaultPageLimit = 100

// Service is the API the desktop shell exposes as traffic.list / traffic.getBody.
type Service struct {
	repo *Repository
}

// NewService wraps a repository.
func NewService(repo *Repository) *Service {
	return &Service{repo: repo}
}

// List returns one page of summaries, newest first, with the total the filter
// matches. An unset page size falls back to DefaultPageLimit and unset page to
// the first page; both are clamped in the repository.
func (s *Service) List(ctx context.Context, filter ListFilter) (TrafficPage, error) {
	if filter.Status != "" && !filter.Status.Valid() {
		return TrafficPage{}, fmt.Errorf("invalid status %q", filter.Status)
	}
	return s.repo.List(ctx, filter)
}

// Delete removes the records with the given ids (traffic.delete). An id that is
// no longer stored is not an error; see Repository.Delete.
func (s *Service) Delete(ctx context.Context, ids []string) (DeleteResult, error) {
	return s.repo.Delete(ctx, ids)
}

// GetBody returns the decompressed request or response body of one record.
func (s *Service) GetBody(ctx context.Context, id string, part BodyPart) ([]byte, error) {
	return s.repo.GetBody(ctx, id, part)
}

// ApplyUpdates writes the settled fields (status, response body, duration) of
// already stored records and returns the number of changed rows. A record that
// carries no response (ResponseBody nil) settles status and duration without
// touching the stored response, so a status/duration-only frame can not erase
// a body already in the store; see Repository.ApplyUpdates.
func (s *Service) ApplyUpdates(ctx context.Context, records []Record) (int, error) {
	return s.repo.ApplyUpdates(ctx, records)
}

// Stats reports the store size, the capture range and the capture path's
// overload counters.
func (s *Service) Stats(ctx context.Context) (Stats, error) {
	return s.repo.Stats(ctx)
}

// AppIDStats groups the store by mini program: one row per appid with its
// record count and the newest capture time (traffic.appids).
func (s *Service) AppIDStats(ctx context.Context) ([]AppIDStat, error) {
	return s.repo.AppIDStats(ctx)
}

// Clear removes every stored record (traffic.clear). The store no longer trims
// itself on a retention policy, so this is the only bulk deletion there is.
func (s *Service) Clear(ctx context.Context) (DeleteResult, error) {
	return s.repo.Clear(ctx)
}
