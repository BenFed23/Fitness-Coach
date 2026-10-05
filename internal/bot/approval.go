package bot

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"

	"fitness-agent/internal/model"
)

type PendingApproval struct {
	Routine   model.RoutineConfig
	CreatedAt time.Time
}

type ApprovalManager struct {
	mu       sync.Mutex
	pendings map[string]PendingApproval
	ttl      time.Duration
}

func NewApprovalManager(ttl time.Duration) *ApprovalManager {
	return &ApprovalManager{
		pendings: make(map[string]PendingApproval),
		ttl:      ttl,
	}
}

// RegisterRoutine adds a new routine pending user approval and returns a unique request ID.
func (m *ApprovalManager) RegisterRoutine(routine model.RoutineConfig) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Clean expired approvals
	now := time.Now()
	for id, item := range m.pendings {
		if now.Sub(item.CreatedAt) > m.ttl {
			delete(m.pendings, id)
		}
	}

	randomBytes := make([]byte, 8)
	_, _ = rand.Read(randomBytes)
	requestID := base64.RawURLEncoding.EncodeToString(randomBytes)

	m.pendings[requestID] = PendingApproval{
		Routine:   routine,
		CreatedAt: now,
	}
	return requestID
}

// Pop retrieves and removes the pending approval by its ID.
func (m *ApprovalManager) Pop(requestID string) (model.RoutineConfig, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	item, exists := m.pendings[requestID]
	if !exists {
		return model.RoutineConfig{}, false
	}
	delete(m.pendings, requestID)

	if time.Since(item.CreatedAt) > m.ttl {
		return model.RoutineConfig{}, false
	}

	return item.Routine, true
}
