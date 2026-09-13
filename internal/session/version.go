package session

import (
	"context"

	"github.com/qs3c/bkcrab/internal/provider"
)

// Optional so non-SQL stores and existing test doubles preserve their contract.
type versionedStore interface {
	GetSessionVersion(context.Context, string, string) ([]provider.Message, int64, error)
	SaveSessionVersion(context.Context, string, string, string, string, string, string, []provider.Message, int64) (int64, error)
}

func (m *Manager) loadWorkingSet(key string) ([]provider.Message, int64, error) {
	if st, ok := m.store.(versionedStore); ok {
		return st.GetSessionVersion(m.ctx(), m.agentID, key)
	}
	msgs, err := m.store.GetSession(m.ctx(), m.agentID, key)
	return msgs, 0, err
}
func (s *Session) saveWorkingSetLocked() error {
	if s.persistenceErr != nil {
		return s.persistenceErr
	}
	var err error
	if st, ok := s.store.(versionedStore); ok {
		var revision int64
		revision, err = st.SaveSessionVersion(s.ctx(), s.agentID, s.sessionKey, s.channel, s.accountID, s.chatID, s.projectID, s.Messages, s.revision)
		if err == nil {
			s.revision = revision
		}
	} else {
		err = s.store.SaveSession(s.ctx(), s.agentID, s.sessionKey, s.channel, s.accountID, s.chatID, s.projectID, s.Messages)
	}
	s.persistenceErr = err
	return err
}
func (s *Session) PersistenceError() error { s.mu.Lock(); defer s.mu.Unlock(); return s.persistenceErr }
