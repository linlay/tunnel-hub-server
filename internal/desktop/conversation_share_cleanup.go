package desktop

import (
	"context"
	"time"

	"example.invalid/tunnel-hub-server/internal/sharefiles"
)

const conversationShareCleanupInterval = time.Hour

func (s *Server) CleanupConversationShareResources(ctx context.Context) error {
	if s.conversationShareResources == nil && s.shareResources == nil {
		return nil
	}
	now := s.now().UTC()
	legacy, err := s.DB.ConversationShareResourceCleanupIDs(ctx, now)
	if err != nil {
		return err
	}
	legacyOwned, err := s.DB.ConversationShareResourceIDs(ctx)
	if err != nil {
		return err
	}
	if err := s.cleanupShareFiles(ctx, s.conversationShareResources, legacy, legacyOwned, s.DB.DeleteConversationShareResourceMetadata, now); err != nil {
		return err
	}
	generic, err := s.DB.ShareResourceCleanupIDs(ctx)
	if err != nil {
		return err
	}
	genericOwned, err := s.DB.ShareResourceIDs(ctx)
	if err != nil {
		return err
	}
	return s.cleanupShareFiles(ctx, s.shareResources, generic, genericOwned, s.DB.DeleteShareResourceMetadata, now)
}

func (s *Server) cleanupShareFiles(ctx context.Context, files *sharefiles.Store, ids []string, owned map[string]struct{}, deleteMetadata func(context.Context, string) error, now time.Time) error {
	if files == nil {
		return nil
	}
	for _, id := range ids {
		if err := files.Delete(id); err != nil {
			s.Logger.Error("delete inactive share resources", "shareId", id, "error", err)
			continue
		}
		if err := deleteMetadata(ctx, id); err != nil {
			s.Logger.Error("delete inactive share metadata", "shareId", id, "error", err)
		}
	}
	return files.CleanupOrphans(owned, now.Add(-time.Hour))
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
