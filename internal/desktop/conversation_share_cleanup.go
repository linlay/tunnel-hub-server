package desktop

import (
	"context"
	"time"
)

const conversationShareCleanupInterval = time.Hour

func (s *Server) CleanupConversationShareResources(ctx context.Context) error {
	if s.conversationShareResources == nil {
		return nil
	}
	now := s.now().UTC()
	ids, err := s.DB.ConversationShareResourceCleanupIDs(ctx, now)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.conversationShareResources.Delete(id); err != nil {
			s.Logger.Error("delete expired conversation share resources", "shareId", id, "error", err)
			continue
		}
		if err := s.DB.DeleteConversationShareResourceMetadata(ctx, id); err != nil {
			s.Logger.Error("delete expired conversation share resource metadata", "shareId", id, "error", err)
		}
	}
	validIDs, err := s.DB.ConversationShareResourceIDs(ctx)
	if err != nil {
		return err
	}
	return s.conversationShareResources.CleanupOrphans(validIDs, now.Add(-time.Hour))
}

func (s *Server) RunConversationShareCleanup(ctx context.Context) {
	if err := s.CleanupConversationShareResources(ctx); err != nil {
		s.Logger.Error("cleanup conversation share resources", "error", err)
	}
	ticker := time.NewTicker(conversationShareCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.CleanupConversationShareResources(ctx); err != nil {
				s.Logger.Error("cleanup conversation share resources", "error", err)
			}
		}
	}
}
